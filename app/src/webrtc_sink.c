#include "webrtc_sink.h"

#include <assert.h>
#include <stdlib.h>
#include <string.h>
#include <libavcodec/avcodec.h>
#include <SDL2/SDL.h>

#include "util/log.h"
#include "util/env.h"
#include "webrtc_signaling.h"
#include "webrtc_connection.h"
#include "webrtc_test_source.h"
#include "controller.h"
#include "control_msg.h"
#include "coords.h"

/** Downcast packet sinks to webrtc_sink */
#define DOWNCAST_VIDEO(SINK) \
    container_of(SINK, struct sc_webrtc_sink, video_packet_sink)
#define DOWNCAST_AUDIO(SINK) \
    container_of(SINK, struct sc_webrtc_sink, audio_packet_sink)

// WebRTC context 結構（包含連接、信令）
struct webrtc_context {
    struct sc_webrtc_connection *connection;
    struct sc_webrtc_signaling *signaling;

    // 連接狀態標記
    bool peer_connected;  // WebRTC peer connection 是否已建立並連接

    // SDP offer/answer 同步機制（Offerer 模式）
    sc_mutex offer_mutex;
    sc_cond offer_cond;
    char *pending_offer;
    bool offer_ready;

    // SDP answer 同步機制（用於保留 Answerer 模式兼容性）
    sc_mutex answer_mutex;
    sc_cond answer_cond;
    char *pending_answer;
    bool answer_ready;

    // WebRTC sink 指針（用於觸發回調）
    struct sc_webrtc_sink *sink;

    // Keyframe 請求標記（在首次發送數據包時請求）
    bool keyframe_requested;

    // 數據包計數器（用於日誌和 keyframe 請求）
    int video_packet_count;
};

static AVPacket *
sc_webrtc_sink_packet_ref(const AVPacket *packet) {
    AVPacket *p = av_packet_alloc();
    if (!p) {
        LOG_OOM();
        return NULL;
    }

    if (av_packet_ref(p, packet)) {
        av_packet_free(&p);
        return NULL;
    }

    return p;
}

static void
sc_webrtc_sink_queue_clear(struct sc_webrtc_sink_queue *queue) {
    while (!sc_vecdeque_is_empty(queue)) {
        AVPacket *p = sc_vecdeque_pop(queue);
        av_packet_free(&p);
    }
}

static bool
sc_webrtc_sink_process_packets(struct sc_webrtc_sink *webrtc_sink,
                               struct webrtc_context *ctx) {
    bool error = false;

    // 測試模式：跳過初始化等待，直接開始生成測試幀
    if (!webrtc_sink->test_mode) {
        // 等待初始化完成
        sc_mutex_lock(&webrtc_sink->mutex);
        while (!webrtc_sink->stopped &&
               (!webrtc_sink->video_init && webrtc_sink->video) &&
               (!webrtc_sink->audio_init && webrtc_sink->audio)) {
            sc_cond_wait(&webrtc_sink->cond, &webrtc_sink->mutex);
        }

        if (webrtc_sink->stopped) {
            sc_mutex_unlock(&webrtc_sink->mutex);
            return false;
        }
        sc_mutex_unlock(&webrtc_sink->mutex);
    }

    LOGI("WebRTC sink: ready to process packets (test_mode=%d)", webrtc_sink->test_mode);

    // 測試模式：等待 WebRTC 連接建立
    if (webrtc_sink->test_mode) {
        LOGI("WebRTC test mode: waiting for peer connection...");
        // 等待連接建立
        while (!webrtc_sink->stopped && !ctx->peer_connected) {
            SDL_Delay(100);  // 每 100ms 檢查一次
        }

        if (webrtc_sink->stopped) {
            return false;
        }

        LOGI("WebRTC test mode: peer connected, starting blue frame generation");
    }

    // 主處理循環
    for (;;) {
        if (webrtc_sink->test_mode) {
            // 測試模式：生成藍色測試幀
            if (webrtc_sink->stopped) {
                break;
            }

            // 30fps = 每 33ms 一幀
            SDL_Delay(33);

            // 生成測試幀
            struct sc_webrtc_test_source *test_source =
                (struct sc_webrtc_test_source *)webrtc_sink->test_source;

            AVPacket *test_pkt = NULL;
            if (sc_webrtc_test_source_generate_frame(test_source, &test_pkt)) {
                ctx->video_packet_count++;

                LOGI("WebRTC test mode: generated frame #%d, size=%d, pts=%" PRIi64,
                     ctx->video_packet_count, test_pkt->size, test_pkt->pts);

                // 只有在 peer connection 已建立時才發送數據
                if (ctx && ctx->peer_connected) {
                    // 同時傳遞 PTS 以設置正確的 RTP timestamp
                    if (!sc_webrtc_connection_send_video_rtp(ctx->connection,
                                                             test_pkt->data,
                                                             test_pkt->size,
                                                             test_pkt->pts)) {
                        LOGW("Failed to send test frame #%d", ctx->video_packet_count);
                    }
                }

                av_packet_free(&test_pkt);
            }

            continue;
        }

        // 正常模式：處理來自 Android 的數據包
        sc_mutex_lock(&webrtc_sink->mutex);

        while (!webrtc_sink->stopped) {
            if ((webrtc_sink->video &&
                 !sc_vecdeque_is_empty(&webrtc_sink->video_queue)) ||
                (webrtc_sink->audio &&
                 !sc_vecdeque_is_empty(&webrtc_sink->audio_queue))) {
                // 有數據包可處理
                break;
            }
            // 等待新的數據包或停止信號
            sc_cond_wait(&webrtc_sink->cond, &webrtc_sink->mutex);
        }

        if (webrtc_sink->stopped) {
            sc_mutex_unlock(&webrtc_sink->mutex);
            break;
        }

        // 獲取視頻包
        AVPacket *video_pkt = NULL;
        if (webrtc_sink->video &&
            !sc_vecdeque_is_empty(&webrtc_sink->video_queue)) {
            video_pkt = sc_vecdeque_pop(&webrtc_sink->video_queue);
        }

        // 獲取音頻包
        AVPacket *audio_pkt = NULL;
        if (webrtc_sink->audio &&
            !sc_vecdeque_is_empty(&webrtc_sink->audio_queue)) {
            audio_pkt = sc_vecdeque_pop(&webrtc_sink->audio_queue);
        }

        sc_mutex_unlock(&webrtc_sink->mutex);

        // 處理視頻包：發送原始 H.264 NAL units 給 libdatachannel
        if (video_pkt) {
            ctx->video_packet_count++;

            // 打印所有視頻封包接收情況（用於調試）
            LOGI("WebRTC sink: received video packet #%d, size=%d, pts=%" PRIi64
                 ", peer_connected=%d",
                 ctx->video_packet_count, video_pkt->size, video_pkt->pts,
                 ctx ? ctx->peer_connected : -1);

            // 只有在 peer connection 已建立時才發送數據
            if (ctx && ctx->peer_connected) {
                // 直接發送整個 AVPacket 數據給 libdatachannel
                // libdatachannel 會自動處理 RTP 封包
                // 同時傳遞 PTS 以設置正確的 RTP timestamp
                if (!sc_webrtc_connection_send_video_rtp(ctx->connection,
                                                         video_pkt->data,
                                                         video_pkt->size,
                                                         video_pkt->pts)) {
                    LOGW("Failed to send video packet #%d", ctx->video_packet_count);
                }
            } else if (ctx && !ctx->peer_connected) {
                // 連接未建立，丟棄數據包（避免日誌過多，只顯示第一次）
                static bool warned = false;
                if (!warned) {
                    LOGD("WebRTC sink: dropping packets, peer not connected yet");
                    warned = true;
                }
            }

            av_packet_free(&video_pkt);
        }

        // 處理音頻包：發送原始音頻數據給 libdatachannel
        if (audio_pkt) {
            LOGV("WebRTC sink: received audio packet, size=%d, pts=%" PRIi64,
                 audio_pkt->size, audio_pkt->pts);

            // 只有在 peer connection 已建立時才發送數據
            if (ctx && ctx->peer_connected) {
                // 直接發送音頻數據給 libdatachannel
                // libdatachannel 的 Opus packetizer 會自動處理 RTP 打包
                if (!sc_webrtc_connection_send_audio_rtp(ctx->connection,
                                                         audio_pkt->data,
                                                         audio_pkt->size)) {
                    LOGV("Failed to send audio packet");
                }
            }

            av_packet_free(&audio_pkt);
        }
    }

    return !error;
}

// ============================================================================
// Data Channel 消息處理（觸控事件）
// ============================================================================

// 簡單的 JSON 字符串提取函數
static char *
json_get_string(const char *json, const char *key) {
    char search_key[128];
    snprintf(search_key, sizeof(search_key), "\"%s\":\"", key);

    const char *start = strstr(json, search_key);
    if (!start) {
        return NULL;
    }

    start += strlen(search_key);
    const char *end = strchr(start, '"');
    if (!end) {
        return NULL;
    }

    size_t len = end - start;
    char *value = malloc(len + 1);
    if (!value) {
        return NULL;
    }

    memcpy(value, start, len);
    value[len] = '\0';
    return value;
}

// 簡單的 JSON 數字提取函數
static bool
json_get_double(const char *json, const char *key, double *out_value) {
    char search_key[128];
    snprintf(search_key, sizeof(search_key), "\"%s\":", key);

    const char *start = strstr(json, search_key);
    if (!start) {
        return false;
    }

    start += strlen(search_key);
    // 跳過空白
    while (*start == ' ' || *start == '\t') {
        start++;
    }

    char *end;
    *out_value = strtod(start, &end);
    return end != start;
}

// 處理來自瀏覽器的觸控事件
static void
on_data_channel_message(struct sc_webrtc_sink *webrtc_sink,
                        const uint8_t *message,
                        size_t size) {
    if (!webrtc_sink->controller) {
        LOGW("WebRTC: received touch event but controller is not available");
        return;
    }

    // C 端節流：最多每 16ms 處理一次觸控事件（約 60 FPS）
    static sc_tick last_touch_time = 0;
    static uint32_t event_count = 0;
    static uint32_t skipped_count = 0;
    event_count++;

    sc_tick now = sc_tick_now();
    sc_tick threshold = SC_TICK_FROM_MS(16);
    sc_tick diff = (last_touch_time > 0) ? (now - last_touch_time) : threshold;

    // 每 30 個事件記錄一次節流狀態
    if (event_count % 30 == 0) {
        LOGD("WebRTC Throttle: event#%u, skipped=%u, now=%lld, last=%lld, diff=%lld, threshold=%lld",
             event_count, skipped_count, (long long)now, (long long)last_touch_time,
             (long long)diff, (long long)threshold);
    }

    if (diff < threshold) {
        // 跳過此事件，頻率太高
        skipped_count++;
        return;
    }
    last_touch_time = now;

    // 使用靜態緩衝區代替 malloc()，避免 OOM 錯誤
    // 最大 JSON 消息應該小於 256 bytes: {"type":"touch","action":"move","x":0.123,"y":0.456,"pointer_id":1}
    static char json_buffer[512];
    if (size >= sizeof(json_buffer)) {
        LOGW("WebRTC: message too large (%zu bytes), max is %zu", size, sizeof(json_buffer) - 1);
        return;
    }

    memcpy(json_buffer, message, size);
    json_buffer[size] = '\0';
    char *json = json_buffer;

    // Print received raw JSON data
    LOGI("Received touch data: %s", json);

    // 解析 JSON 消息
    char *type = json_get_string(json, "type");
    if (!type || strcmp(type, "touch") != 0) {
        LOGW("WebRTC: unknown message type: %s", type ? type : "(null)");
        free(type);
        return;
    }
    free(type);

    char *action_str = json_get_string(json, "action");
    double x, y, pointer_id_double;

    if (!action_str || !json_get_double(json, "x", &x) ||
        !json_get_double(json, "y", &y) ||
        !json_get_double(json, "pointer_id", &pointer_id_double)) {
        LOGW("WebRTC: invalid touch message format");
        free(action_str);
        return;
    }

    // Print parsed data
    LOGI("Parsed touch: action=%s, x=%.4f, y=%.4f, pointer_id=%.0f",
         action_str, x, y, pointer_id_double);

    // 將 action 字符串轉換為 Android 枚舉
    enum android_motionevent_action action;
    const char *action_name;
    if (strcmp(action_str, "down") == 0) {
        action = AMOTION_EVENT_ACTION_DOWN;
        action_name = "DOWN";
    } else if (strcmp(action_str, "move") == 0) {
        action = AMOTION_EVENT_ACTION_MOVE;
        action_name = "MOVE";
    } else if (strcmp(action_str, "up") == 0) {
        action = AMOTION_EVENT_ACTION_UP;
        action_name = "UP";
    } else {
        LOGW("WebRTC: unknown action: %s", action_str);
        free(action_str);
        return;
    }
    free(action_str);

    // 構造控制消息
    struct sc_control_msg msg;
    msg.type = SC_CONTROL_MSG_TYPE_INJECT_TOUCH_EVENT;
    msg.inject_touch_event.action = action;
    msg.inject_touch_event.pointer_id = (uint64_t)pointer_id_double;

    // 座標是相對座標（0-1），需要轉換為絕對座標
    // 使用實際視訊尺寸進行轉換（與 SDL 觸控行為一致）
    msg.inject_touch_event.position.point.x = (int32_t)(x * webrtc_sink->video_width);
    msg.inject_touch_event.position.point.y = (int32_t)(y * webrtc_sink->video_height);
    msg.inject_touch_event.position.screen_size.width = webrtc_sink->video_width;
    msg.inject_touch_event.position.screen_size.height = webrtc_sink->video_height;

    // Pressure: UP 動作時為 0.0，DOWN/MOVE 時為 1.0
    bool up = action == AMOTION_EVENT_ACTION_UP;
    msg.inject_touch_event.pressure = up ? 0.0f : 1.0f;
    msg.inject_touch_event.action_button = 0;
    msg.inject_touch_event.buttons = 0;

    // Print final injected touch event (with converted coordinates)
    LOGI("Injecting touch: %s [%d], relative(%.4f, %.4f), absolute(%d, %d), pointer_id=%" PRIu64,
         action_name, action, x, y,
         msg.inject_touch_event.position.point.x,
         msg.inject_touch_event.position.point.y,
         msg.inject_touch_event.pointer_id);

    // 發送控制消息
    if (!sc_controller_push_msg(webrtc_sink->controller, &msg)) {
        LOGW("WebRTC: failed to push touch event to controller");
    }
}

// ============================================================================
// 信令服務器回調函數
// ============================================================================

static char *
on_signaling_offer(struct sc_webrtc_signaling *signaling,
                  const char *offer_json,
                  void *userdata) {
    struct webrtc_context *ctx = userdata;
    (void) signaling;

    LOGI("Signaling: received SDP offer from browser (len=%zu)",
         strlen(offer_json));
    LOGD("Offer JSON: %.200s...", offer_json);  // 只顯示前 200 字符

    if (!ctx->connection) {
        LOGE("WebRTC connection not initialized");
        return NULL;
    }

    // 簡單解析 JSON 提取 SDP（查找 "sdp":"..." 部分）
    const char *sdp_start = strstr(offer_json, "\"sdp\":\"");
    if (!sdp_start) {
        LOGE("Failed to find SDP in offer JSON");
        return NULL;
    }
    sdp_start += 7;  // 跳過 "sdp":"

    const char *sdp_end = sdp_start;
    while (*sdp_end && *sdp_end != '"') {
        sdp_end++;
    }

    size_t sdp_len = sdp_end - sdp_start;
    char *sdp = malloc(sdp_len * 2 + 1);  // 預留空間處理轉義字符
    if (!sdp) {
        LOG_OOM();
        return NULL;
    }

    // 解碼轉義字符 (\r\n -> 真實換行)
    size_t j = 0;
    for (size_t i = 0; i < sdp_len; i++) {
        if (sdp_start[i] == '\\' && i + 1 < sdp_len) {
            if (sdp_start[i + 1] == 'r') {
                sdp[j++] = '\r';
                i++;
            } else if (sdp_start[i + 1] == 'n') {
                sdp[j++] = '\n';
                i++;
            } else if (sdp_start[i + 1] == '"') {
                sdp[j++] = '"';
                i++;
            } else if (sdp_start[i + 1] == '\\') {
                sdp[j++] = '\\';
                i++;
            } else {
                sdp[j++] = sdp_start[i];
            }
        } else {
            sdp[j++] = sdp_start[i];
        }
    }
    sdp[j] = '\0';

    LOGI("Extracted SDP offer (len=%zu)", strlen(sdp));
    LOGD("SDP offer content:\n%s", sdp);

    // 重置 answer 狀態
    sc_mutex_lock(&ctx->answer_mutex);
    free(ctx->pending_answer);
    ctx->pending_answer = NULL;
    ctx->answer_ready = false;
    sc_mutex_unlock(&ctx->answer_mutex);

    // 設置遠程描述（瀏覽器的 offer）
    LOGD("Calling sc_webrtc_connection_set_remote_description...");
    bool success = sc_webrtc_connection_set_remote_description(ctx->connection,
                                                               "offer", sdp);
    free(sdp);

    if (!success) {
        LOGE("Failed to set remote description");
        return NULL;
    }
    LOGD("sc_webrtc_connection_set_remote_description succeeded");

    // Tracks 已經在連接初始化時添加,現在直接創建 answer
    // 創建 answer（異步操作）
    LOGD("Calling sc_webrtc_connection_create_answer...");
    if (!sc_webrtc_connection_create_answer(ctx->connection)) {
        LOGE("Failed to create answer");
        return NULL;
    }

    // 等待 answer 生成（最多 5 秒）
    sc_mutex_lock(&ctx->answer_mutex);
    sc_tick deadline = sc_tick_now() + SC_TICK_FROM_SEC(5);
    while (!ctx->answer_ready) {
        if (!sc_cond_timedwait(&ctx->answer_cond, &ctx->answer_mutex, deadline)) {
            LOGE("Timeout waiting for SDP answer");
            sc_mutex_unlock(&ctx->answer_mutex);
            return NULL;
        }
    }

    char *answer_sdp = ctx->pending_answer;
    ctx->pending_answer = NULL;  // 轉移所有權給調用者
    ctx->answer_ready = false;
    sc_mutex_unlock(&ctx->answer_mutex);

    if (!answer_sdp) {
        LOGE("Answer SDP is NULL");
        return NULL;
    }

    // 將 SDP 包裝成 JSON 格式: {"type":"answer","sdp":"..."}
    // 需要轉義換行符 \r\n
    size_t answer_sdp_len = strlen(answer_sdp);
    size_t json_size = answer_sdp_len * 2 + 100;  // 預留空間給轉義和 JSON 結構
    char *answer_json = malloc(json_size);
    if (!answer_json) {
        LOG_OOM();
        free(answer_sdp);
        return NULL;
    }

    // 開始構建 JSON
    size_t pos = 0;
    pos += snprintf(answer_json + pos, json_size - pos, "{\"type\":\"answer\",\"sdp\":\"");

    // 轉義 SDP 內容
    for (size_t i = 0; i < answer_sdp_len && pos < json_size - 10; i++) {
        if (answer_sdp[i] == '\r') {
            answer_json[pos++] = '\\';
            answer_json[pos++] = 'r';
        } else if (answer_sdp[i] == '\n') {
            answer_json[pos++] = '\\';
            answer_json[pos++] = 'n';
        } else if (answer_sdp[i] == '"') {
            answer_json[pos++] = '\\';
            answer_json[pos++] = '"';
        } else if (answer_sdp[i] == '\\') {
            answer_json[pos++] = '\\';
            answer_json[pos++] = '\\';
        } else {
            answer_json[pos++] = answer_sdp[i];
        }
    }

    pos += snprintf(answer_json + pos, json_size - pos, "\"}");
    answer_json[pos] = '\0';

    free(answer_sdp);

    LOGI("Signaling: returning SDP answer to browser (JSON len=%zu)", strlen(answer_json));
    LOGD("Answer JSON (first 500 chars): %.500s", answer_json);
    return answer_json;
}

static char *
on_signaling_get_offer(struct sc_webrtc_signaling *signaling,
                       void *userdata) {
    struct webrtc_context *ctx = userdata;
    (void) signaling;

    LOGI("Signaling: browser requesting SDP offer");

    if (!ctx->connection) {
        LOGE("WebRTC connection not initialized");
        return NULL;
    }

    // 等待 offer 準備好（最多 5 秒）
    sc_mutex_lock(&ctx->offer_mutex);
    if (!ctx->offer_ready) {
        LOGW("Offer not ready yet, waiting...");
        sc_tick deadline = sc_tick_now() + SC_TICK_FROM_SEC(5);
        while (!ctx->offer_ready) {
            if (!sc_cond_timedwait(&ctx->offer_cond, &ctx->offer_mutex, deadline)) {
                LOGE("Timeout waiting for SDP offer");
                sc_mutex_unlock(&ctx->offer_mutex);
                return NULL;
            }
        }
    }

    char *offer_sdp = ctx->pending_offer;
    ctx->pending_offer = NULL;  // 轉移所有權給調用者（不清除 offer_ready，允許多次獲取）
    sc_mutex_unlock(&ctx->offer_mutex);

    if (!offer_sdp) {
        LOGE("Offer SDP is NULL");
        return NULL;
    }

    // 將 SDP 包裝成 JSON 格式: {"type":"offer","sdp":"..."}
    size_t offer_sdp_len = strlen(offer_sdp);
    size_t json_size = offer_sdp_len * 2 + 100;
    char *offer_json = malloc(json_size);
    if (!offer_json) {
        LOG_OOM();
        free(offer_sdp);
        return NULL;
    }

    // 構建 JSON 並轉義換行符
    size_t pos = 0;
    pos += snprintf(offer_json + pos, json_size - pos, "{\"type\":\"offer\",\"sdp\":\"");

    for (size_t i = 0; i < offer_sdp_len && pos < json_size - 10; i++) {
        if (offer_sdp[i] == '\r') {
            offer_json[pos++] = '\\';
            offer_json[pos++] = 'r';
        } else if (offer_sdp[i] == '\n') {
            offer_json[pos++] = '\\';
            offer_json[pos++] = 'n';
        } else if (offer_sdp[i] == '"') {
            offer_json[pos++] = '\\';
            offer_json[pos++] = '"';
        } else if (offer_sdp[i] == '\\') {
            offer_json[pos++] = '\\';
            offer_json[pos++] = '\\';
        } else {
            offer_json[pos++] = offer_sdp[i];
        }
    }

    pos += snprintf(offer_json + pos, json_size - pos, "\"}");
    offer_json[pos] = '\0';

    free(offer_sdp);

    LOGI("Signaling: returning SDP offer to browser (JSON len=%zu)", strlen(offer_json));
    return offer_json;
}

static void
on_signaling_answer(struct sc_webrtc_signaling *signaling,
                    const char *answer_json,
                    void *userdata) {
    struct webrtc_context *ctx = userdata;
    (void) signaling;

    LOGI("Signaling: received SDP answer from browser (len=%zu)",
         strlen(answer_json));
    LOGD("Answer JSON: %.200s...", answer_json);

    if (!ctx->connection) {
        LOGE("WebRTC connection not initialized");
        return;
    }

    // 解析 JSON 提取 SDP
    const char *sdp_start = strstr(answer_json, "\"sdp\":\"");
    if (!sdp_start) {
        LOGE("Failed to find SDP in answer JSON");
        return;
    }
    sdp_start += 7;  // 跳過 "sdp":"

    const char *sdp_end = sdp_start;
    while (*sdp_end && *sdp_end != '"') {
        sdp_end++;
    }

    size_t sdp_len = sdp_end - sdp_start;
    char *sdp = malloc(sdp_len * 2 + 1);
    if (!sdp) {
        LOG_OOM();
        return;
    }

    // 解碼轉義字符
    size_t j = 0;
    for (size_t i = 0; i < sdp_len; i++) {
        if (sdp_start[i] == '\\' && i + 1 < sdp_len) {
            if (sdp_start[i + 1] == 'r') {
                sdp[j++] = '\r';
                i++;
            } else if (sdp_start[i + 1] == 'n') {
                sdp[j++] = '\n';
                i++;
            } else if (sdp_start[i + 1] == '"') {
                sdp[j++] = '"';
                i++;
            } else if (sdp_start[i + 1] == '\\') {
                sdp[j++] = '\\';
                i++;
            } else {
                sdp[j++] = sdp_start[i];
            }
        } else {
            sdp[j++] = sdp_start[i];
        }
    }
    sdp[j] = '\0';

    LOGI("Extracted SDP answer (len=%zu)", strlen(sdp));
    LOGD("SDP answer content:\n%s", sdp);

    // 設置遠程描述（瀏覽器的 answer）
    LOGD("Calling sc_webrtc_connection_set_remote_description...");
    bool success = sc_webrtc_connection_set_remote_description(ctx->connection,
                                                               "answer", sdp);
    free(sdp);

    if (!success) {
        LOGE("Failed to set remote description (answer)");
        return;
    }

    LOGI("SDP answer set successfully");
}

static void
on_signaling_ice_candidate(struct sc_webrtc_signaling *signaling,
                          const char *candidate_json,
                          void *userdata) {
    struct webrtc_context *ctx = userdata;
    (void) signaling;

    LOGD("Signaling: received ICE candidate from browser");
    LOGD("ICE candidate JSON: %s", candidate_json);

    if (!ctx->connection) {
        LOGE("WebRTC connection not initialized");
        return;
    }

    // 解析 JSON: {"candidate":"...","sdpMid":"...","sdpMLineIndex":...}
    const char *candidate_str = strstr(candidate_json, "\"candidate\":\"");
    if (!candidate_str) {
        LOGE("Failed to find 'candidate' field in JSON");
        return;
    }
    candidate_str += 13;  // 跳過 "candidate":"

    const char *candidate_end = strchr(candidate_str, '"');
    if (!candidate_end) {
        LOGE("Failed to parse candidate string");
        return;
    }

    size_t candidate_len = candidate_end - candidate_str;
    char *candidate = malloc(candidate_len + 1);
    if (!candidate) {
        LOG_OOM();
        return;
    }
    memcpy(candidate, candidate_str, candidate_len);
    candidate[candidate_len] = '\0';

    // 解析 sdpMid
    char *sdp_mid = NULL;
    const char *mid_str = strstr(candidate_json, "\"sdpMid\":\"");
    if (mid_str) {
        mid_str += 10;  // 跳過 "sdpMid":"
        const char *mid_end = strchr(mid_str, '"');
        if (mid_end) {
            size_t mid_len = mid_end - mid_str;
            sdp_mid = malloc(mid_len + 1);
            if (sdp_mid) {
                memcpy(sdp_mid, mid_str, mid_len);
                sdp_mid[mid_len] = '\0';
            }
        }
    }

    LOGD("Parsed candidate: %s, sdpMid: %s", candidate,
         sdp_mid ? sdp_mid : "(null)");

    // 添加遠程 ICE candidate
    if (!sc_webrtc_connection_add_ice_candidate(ctx->connection, candidate,
                                                sdp_mid)) {
        LOGW("Failed to add ICE candidate");
    }

    free(candidate);
    free(sdp_mid);
}

static void
on_signaling_client_connected(struct sc_webrtc_signaling *signaling,
                              void *userdata) {
    (void) signaling;
    (void) userdata;

    LOGI("Signaling: client connected");
}

static void
on_signaling_client_disconnected(struct sc_webrtc_signaling *signaling,
                                 void *userdata) {
    (void) signaling;
    (void) userdata;

    LOGI("Signaling: client disconnected");
}

// ============================================================================
// WebRTC 連接回調函數
// ============================================================================

static void
on_webrtc_local_description(struct sc_webrtc_connection *connection,
                           const char *type,
                           const char *sdp,
                           void *userdata) {
    struct webrtc_context *ctx = userdata;
    (void) connection;

    LOGI("WebRTC: local %s created", type);

    // Offerer 模式：如果是 offer，存儲並通知等待的線程
    if (strcmp(type, "offer") == 0) {
        sc_mutex_lock(&ctx->offer_mutex);
        free(ctx->pending_offer);
        ctx->pending_offer = strdup(sdp);
        ctx->offer_ready = true;
        sc_cond_signal(&ctx->offer_cond);
        sc_mutex_unlock(&ctx->offer_mutex);

        LOGI("WebRTC: SDP offer ready, signaled waiting thread");
    }
    // Answerer 模式：如果是 answer，存儲並通知等待的線程
    else if (strcmp(type, "answer") == 0) {
        sc_mutex_lock(&ctx->answer_mutex);
        free(ctx->pending_answer);
        ctx->pending_answer = strdup(sdp);
        ctx->answer_ready = true;
        sc_cond_signal(&ctx->answer_cond);
        sc_mutex_unlock(&ctx->answer_mutex);

        LOGI("WebRTC: SDP answer ready, signaled waiting thread");
    }
}

static void
on_webrtc_ice_candidate(struct sc_webrtc_connection *connection,
                       const char *candidate,
                       const char *sdp_mid,
                       void *userdata) {
    struct webrtc_context *ctx = userdata;
    (void) connection;

    LOGD("WebRTC: ICE candidate generated, mid=%s, candidate=%s", sdp_mid, candidate);

    // 通過信令服務器發送 ICE candidate 給瀏覽器
    if (ctx->signaling) {
        sc_webrtc_signaling_send_ice_candidate(ctx->signaling, candidate, sdp_mid);
    }
}

static void
on_webrtc_state_change(struct sc_webrtc_connection *connection,
                      enum sc_webrtc_connection_state state,
                      void *userdata) {
    (void) connection;
    struct webrtc_context *ctx = userdata;

    const char *state_str = "";
    switch (state) {
        case SC_WEBRTC_STATE_NEW:
            state_str = "NEW";
            ctx->peer_connected = false;
            break;
        case SC_WEBRTC_STATE_CONNECTING:
            state_str = "CONNECTING";
            ctx->peer_connected = false;
            break;
        case SC_WEBRTC_STATE_CONNECTED:
            state_str = "CONNECTED";
            ctx->peer_connected = true;
            LOGI("WebRTC: peer connection established, starting to send media data");

            // 立即請求關鍵幀，確保有新的視頻數據開始傳輸
            // 這解決了時序問題：之前的視頻封包在連接建立前已被丟棄
            LOGI("WebRTC: requesting keyframe immediately after connection established");
            if (ctx->sink && ctx->sink->cbs && ctx->sink->cbs->on_request_keyframe) {
                ctx->sink->cbs->on_request_keyframe(ctx->sink, ctx->sink->cbs_userdata);
            }

            // 觸發 WebRTC sink 的 on_connected 回調
            if (ctx->sink && ctx->sink->cbs && ctx->sink->cbs->on_connected) {
                ctx->sink->cbs->on_connected(ctx->sink, ctx->sink->cbs_userdata);
            }
            break;
        case SC_WEBRTC_STATE_DISCONNECTED:
            state_str = "DISCONNECTED";
            ctx->peer_connected = false;

            // 觸發 WebRTC sink 的 on_disconnected 回調
            if (ctx->sink && ctx->sink->cbs && ctx->sink->cbs->on_disconnected) {
                ctx->sink->cbs->on_disconnected(ctx->sink, ctx->sink->cbs_userdata);
            }
            break;
        case SC_WEBRTC_STATE_FAILED:
            state_str = "FAILED";
            ctx->peer_connected = false;
            break;
        case SC_WEBRTC_STATE_CLOSED:
            state_str = "CLOSED";
            ctx->peer_connected = false;
            break;
    }

    LOGI("WebRTC: connection state changed to %s", state_str);
}

static void
on_webrtc_error(struct sc_webrtc_connection *connection,
               const char *error,
               void *userdata) {
    (void) connection;
    (void) userdata;

    LOGE("WebRTC error: %s", error);
}

static void
on_webrtc_keyframe_request(struct sc_webrtc_connection *connection,
                          void *userdata) {
    (void) connection;
    struct webrtc_context *ctx = userdata;

    LOGI("WebRTC: keyframe requested (PLI/FIR or error recovery)");

    // 轉發請求到 WebRTC sink 的回調
    if (ctx->sink && ctx->sink->cbs && ctx->sink->cbs->on_request_keyframe) {
        ctx->sink->cbs->on_request_keyframe(ctx->sink, ctx->sink->cbs_userdata);
    }
}

static void
on_webrtc_data_channel_message(struct sc_webrtc_connection *connection,
                               const uint8_t *message,
                               size_t size,
                               void *userdata) {
    (void) connection;
    struct webrtc_context *ctx = userdata;

    // 轉發到 webrtc_sink 處理
    if (ctx->sink) {
        on_data_channel_message(ctx->sink, message, size);
    }
}

static int
run_webrtc_sink(void *data) {
    struct sc_webrtc_sink *webrtc_sink = data;

    LOGI("WebRTC sink thread started");

    // 檢查是否啟用測試模式
    char *env_test = sc_get_env("SCRCPY_WEBRTC_TEST");
    bool test_mode = env_test && (strcmp(env_test, "1") == 0 ||
                                  strcmp(env_test, "true") == 0 ||
                                  strcmp(env_test, "TRUE") == 0);
    free(env_test);

    // 臨時：強制啟用測試模式（已關閉，使用真實 Android 串流）
    // test_mode = true;
    // LOGI("*** FORCED TEST MODE ENABLED FOR DEBUGGING ***");

    struct sc_webrtc_test_source test_source;
    bool test_source_initialized = false;

    if (test_mode) {
        LOGI("WebRTC test mode enabled - will generate blue screen");
        // 初始化測試視頻源（720p @ 30fps）
        if (sc_webrtc_test_source_init(&test_source, 720, 1280, 30)) {
            test_source_initialized = true;
            webrtc_sink->test_mode = true;
            webrtc_sink->test_source = &test_source;
            LOGI("WebRTC test source initialized successfully");
        } else {
            LOGE("Failed to initialize WebRTC test source");
            return 0;
        }
    }

    // 首先創建 context（用於回調）
    struct webrtc_context ctx = {
        .connection = NULL,
        .signaling = NULL,
        .peer_connected = false,  // 初始狀態：未連接
        .pending_answer = NULL,
        .answer_ready = false,
        .sink = webrtc_sink,  // 保存 sink 指針以便觸發回調
        .keyframe_requested = false,  // 初始狀態：未請求 keyframe
        .video_packet_count = 0,  // 視頻包計數器
    };

    // 初始化同步機制（Offerer 模式和 Answerer 模式）
    bool offer_mutex_ok = sc_mutex_init(&ctx.offer_mutex);
    bool offer_cond_ok = sc_cond_init(&ctx.offer_cond);
    bool answer_mutex_ok = sc_mutex_init(&ctx.answer_mutex);
    bool answer_cond_ok = sc_cond_init(&ctx.answer_cond);

    if (!offer_mutex_ok || !offer_cond_ok || !answer_mutex_ok || !answer_cond_ok) {
        LOGE("Failed to initialize synchronization primitives");
        if (offer_mutex_ok) sc_mutex_destroy(&ctx.offer_mutex);
        if (offer_cond_ok) sc_cond_destroy(&ctx.offer_cond);
        if (answer_mutex_ok) sc_mutex_destroy(&ctx.answer_mutex);
        if (answer_cond_ok) sc_cond_destroy(&ctx.answer_cond);
        return 0;
    }

    // 階段3.1 - 啟動 HTTP 信令服務器（帶回調）
    struct sc_webrtc_signaling_callbacks signaling_cbs = {
        .on_offer = on_signaling_offer,  // 保留 Answerer 模式兼容性
        .on_get_offer = on_signaling_get_offer,  // Offerer 模式：返回本地 offer
        .on_answer = on_signaling_answer,  // Offerer 模式：處理遠端 answer
        .on_ice_candidate = on_signaling_ice_candidate,
        .on_client_connected = on_signaling_client_connected,
        .on_client_disconnected = on_signaling_client_disconnected,
    };

    struct sc_webrtc_signaling *signaling = NULL;
    bool signaling_ok = sc_webrtc_signaling_init(&signaling,
                                                 webrtc_sink->http_port,
                                                 &signaling_cbs, &ctx);
    if (signaling_ok) {
        ctx.signaling = signaling;  // 更新 context
        signaling_ok = sc_webrtc_signaling_start(signaling);
        if (signaling_ok) {
            webrtc_sink->signaling_server = signaling;
            const char *url = sc_webrtc_signaling_get_url(signaling,
                                                          webrtc_sink->http_port);
            LOGI("WebRTC player available at: %s", url);
        }
    }

    if (!signaling_ok) {
        LOGW("Failed to start signaling server, continuing without it");
    }

    struct sc_webrtc_connection_callbacks webrtc_cbs = {
        .on_local_description = on_webrtc_local_description,
        .on_ice_candidate = on_webrtc_ice_candidate,
        .on_connection_state_change = on_webrtc_state_change,
        .on_data_channel_open = NULL,
        .on_error = on_webrtc_error,
        .on_keyframe_request = on_webrtc_keyframe_request,
        .on_data_channel_message = on_webrtc_data_channel_message,
    };

    struct sc_webrtc_connection_config webrtc_config = {
        .stun_server = webrtc_sink->stun_server,
        .turn_server = NULL,
        .turn_username = NULL,
        .turn_password = NULL,
        .video_enabled = webrtc_sink->video,
        .audio_enabled = webrtc_sink->audio,
    };

    bool webrtc_ok = sc_webrtc_connection_init(&ctx.connection, &webrtc_config,
                                               &webrtc_cbs, &ctx);
    if (webrtc_ok) {
        webrtc_sink->webrtc_context = ctx.connection;

        // 主動創建 tracks（在收到 offer 之前）
        // 這樣可以在 track 創建時立即配置 packetizers
        // 類似 Go/Pion 實現：先創建 tracks，再處理 offer
        LOGI("Proactively creating media tracks before offer negotiation");

        if (webrtc_config.video_enabled) {
            if (!sc_webrtc_connection_add_video_track(ctx.connection, "video")) {
                LOGE("Failed to add video track");
                webrtc_ok = false;
            } else {
                LOGI("Video track created and H.264 packetizer configured");
            }
        }

        if (webrtc_ok && webrtc_config.audio_enabled) {
            if (!sc_webrtc_connection_add_audio_track(ctx.connection, "audio")) {
                LOGE("Failed to add audio track");
                webrtc_ok = false;
            } else {
                LOGI("Audio track created and Opus packetizer configured");
            }
        }

        // 創建 data channel（必須在添加所有 tracks 之後，創建 offer 之前）
        // 注意：rtcCreateDataChannel 會觸發 SDP 生成，所以必須在 tracks 都加完之後調用
        if (webrtc_ok && webrtc_sink->controller) {
            LOGI("Creating data channel for touch control");
            if (!sc_webrtc_connection_create_data_channel(ctx.connection, "control")) {
                LOGE("Failed to create data channel");
                webrtc_ok = false;
            } else {
                LOGI("Data channel created successfully");
            }
        }

        if (webrtc_ok) {
            LOGI("WebRTC connection ready with configured packetizers, creating offer (Offerer mode)");

            // Offerer 模式：創建 offer
            if (!sc_webrtc_connection_create_offer(ctx.connection)) {
                LOGE("Failed to create offer");
                webrtc_ok = false;
            } else {
                LOGI("SDP offer creation initiated");
            }
        }
    } else {
        LOGW("Failed to initialize WebRTC connection");
    }

    bool success = sc_webrtc_sink_process_packets(webrtc_sink, &ctx);

    sc_mutex_lock(&webrtc_sink->mutex);
    // 防止生產者推送新的數據包
    webrtc_sink->stopped = true;
    // 丟棄待處理的數據包
    sc_webrtc_sink_queue_clear(&webrtc_sink->video_queue);
    sc_webrtc_sink_queue_clear(&webrtc_sink->audio_queue);
    sc_mutex_unlock(&webrtc_sink->mutex);

    // 清理 WebRTC 連接
    if (ctx.connection) {
        sc_webrtc_connection_destroy(ctx.connection);
        webrtc_sink->webrtc_context = NULL;
    }

    // 階段3.1 - 停止 HTTP 信令服務器
    if (signaling) {
        sc_webrtc_signaling_stop(signaling);
        sc_webrtc_signaling_destroy(signaling);
        webrtc_sink->signaling_server = NULL;
    }

    // 清理同步機制
    free(ctx.pending_offer);
    sc_cond_destroy(&ctx.offer_cond);
    sc_mutex_destroy(&ctx.offer_mutex);
    free(ctx.pending_answer);
    sc_cond_destroy(&ctx.answer_cond);
    sc_mutex_destroy(&ctx.answer_mutex);

    // 清理測試源
    if (test_source_initialized) {
        LOGI("Destroying WebRTC test source");
        sc_webrtc_test_source_destroy(&test_source);
        webrtc_sink->test_mode = false;
        webrtc_sink->test_source = NULL;
    }

    if (success) {
        LOGI("WebRTC sink completed successfully");
    } else {
        LOGE("WebRTC sink failed");
    }

    LOGD("WebRTC sink thread ended");

    if (webrtc_sink->cbs && webrtc_sink->cbs->on_disconnected) {
        webrtc_sink->cbs->on_disconnected(webrtc_sink, webrtc_sink->cbs_userdata);
    }

    return 0;
}

// ============================================================================
// Packet sink 視頻接口實現
// ============================================================================

static bool
sc_webrtc_sink_video_packet_sink_open(struct sc_packet_sink *sink,
                                      AVCodecContext *ctx) {
    struct sc_webrtc_sink *webrtc_sink = DOWNCAST_VIDEO(sink);
    assert(!webrtc_sink->video_init);

    sc_mutex_lock(&webrtc_sink->mutex);

    if (webrtc_sink->stopped) {
        sc_mutex_unlock(&webrtc_sink->mutex);
        return false;
    }

    LOGI("WebRTC sink: video opened, codec=%s, size=%dx%d",
         avcodec_get_name(ctx->codec_id), ctx->width, ctx->height);

    // 保存視訊尺寸（用於觸控座標轉換）
    webrtc_sink->video_width = ctx->width;
    webrtc_sink->video_height = ctx->height;

    // TODO: 階段3.3 - 初始化 RTP 打包器（視頻）

    webrtc_sink->video_init = true;
    sc_cond_signal(&webrtc_sink->cond);
    sc_mutex_unlock(&webrtc_sink->mutex);

    return true;
}

static void
sc_webrtc_sink_video_packet_sink_close(struct sc_packet_sink *sink) {
    struct sc_webrtc_sink *webrtc_sink = DOWNCAST_VIDEO(sink);
    assert(webrtc_sink->video_init);

    sc_mutex_lock(&webrtc_sink->mutex);
    webrtc_sink->stopped = true;
    sc_cond_signal(&webrtc_sink->cond);
    sc_mutex_unlock(&webrtc_sink->mutex);

    LOGI("WebRTC sink: video closed");
}

static bool
sc_webrtc_sink_video_packet_sink_push(struct sc_packet_sink *sink,
                                      const AVPacket *packet) {
    struct sc_webrtc_sink *webrtc_sink = DOWNCAST_VIDEO(sink);
    assert(webrtc_sink->video_init);

    sc_mutex_lock(&webrtc_sink->mutex);

    if (webrtc_sink->stopped) {
        sc_mutex_unlock(&webrtc_sink->mutex);
        return false;
    }

    AVPacket *pkt = sc_webrtc_sink_packet_ref(packet);
    if (!pkt) {
        LOG_OOM();
        sc_mutex_unlock(&webrtc_sink->mutex);
        return false;
    }

    bool ok = sc_vecdeque_push(&webrtc_sink->video_queue, pkt);
    if (!ok) {
        LOG_OOM();
        av_packet_free(&pkt);
        sc_mutex_unlock(&webrtc_sink->mutex);
        return false;
    }

    sc_cond_signal(&webrtc_sink->cond);
    sc_mutex_unlock(&webrtc_sink->mutex);

    return true;
}

// ============================================================================
// Packet sink 音頻接口實現
// ============================================================================

static bool
sc_webrtc_sink_audio_packet_sink_open(struct sc_packet_sink *sink,
                                      AVCodecContext *ctx) {
    struct sc_webrtc_sink *webrtc_sink = DOWNCAST_AUDIO(sink);
    assert(webrtc_sink->audio);
    assert(!webrtc_sink->audio_init);

    sc_mutex_lock(&webrtc_sink->mutex);

    if (webrtc_sink->stopped) {
        sc_mutex_unlock(&webrtc_sink->mutex);
        return false;
    }

    LOGI("WebRTC sink: audio opened, codec=%s, sample_rate=%d, channels=%d",
         avcodec_get_name(ctx->codec_id), ctx->sample_rate, ctx->ch_layout.nb_channels);

    // TODO: 階段3.3 - 初始化 RTP 打包器（音頻）

    webrtc_sink->audio_init = true;
    sc_cond_signal(&webrtc_sink->cond);
    sc_mutex_unlock(&webrtc_sink->mutex);

    return true;
}

static void
sc_webrtc_sink_audio_packet_sink_close(struct sc_packet_sink *sink) {
    struct sc_webrtc_sink *webrtc_sink = DOWNCAST_AUDIO(sink);
    assert(webrtc_sink->audio);
    assert(webrtc_sink->audio_init);

    sc_mutex_lock(&webrtc_sink->mutex);
    webrtc_sink->stopped = true;
    sc_cond_signal(&webrtc_sink->cond);
    sc_mutex_unlock(&webrtc_sink->mutex);

    LOGI("WebRTC sink: audio closed");
}

static bool
sc_webrtc_sink_audio_packet_sink_push(struct sc_packet_sink *sink,
                                      const AVPacket *packet) {
    struct sc_webrtc_sink *webrtc_sink = DOWNCAST_AUDIO(sink);
    assert(webrtc_sink->audio);
    assert(webrtc_sink->audio_init);

    sc_mutex_lock(&webrtc_sink->mutex);

    if (webrtc_sink->stopped) {
        sc_mutex_unlock(&webrtc_sink->mutex);
        return false;
    }

    AVPacket *pkt = sc_webrtc_sink_packet_ref(packet);
    if (!pkt) {
        LOG_OOM();
        sc_mutex_unlock(&webrtc_sink->mutex);
        return false;
    }

    bool ok = sc_vecdeque_push(&webrtc_sink->audio_queue, pkt);
    if (!ok) {
        LOG_OOM();
        av_packet_free(&pkt);
        sc_mutex_unlock(&webrtc_sink->mutex);
        return false;
    }

    sc_cond_signal(&webrtc_sink->cond);
    sc_mutex_unlock(&webrtc_sink->mutex);

    return true;
}

static void
sc_webrtc_sink_audio_packet_sink_disable(struct sc_packet_sink *sink) {
    struct sc_webrtc_sink *webrtc_sink = DOWNCAST_AUDIO(sink);
    assert(webrtc_sink->audio);
    assert(!webrtc_sink->audio_init);

    LOGW("WebRTC sink: audio disabled");

    sc_mutex_lock(&webrtc_sink->mutex);
    webrtc_sink->audio = false;
    webrtc_sink->audio_init = true;
    sc_cond_signal(&webrtc_sink->cond);
    sc_mutex_unlock(&webrtc_sink->mutex);
}

// ============================================================================
// 生命週期管理函數
// ============================================================================

bool
sc_webrtc_sink_init(struct sc_webrtc_sink *webrtc_sink, uint16_t http_port,
                    const char *stun_server, bool video, bool audio,
                    struct sc_controller *controller,
                    const struct sc_webrtc_sink_callbacks *cbs,
                    void *cbs_userdata) {
    assert(video || audio);

    webrtc_sink->http_port = http_port;

    if (stun_server) {
        webrtc_sink->stun_server = strdup(stun_server);
        if (!webrtc_sink->stun_server) {
            LOG_OOM();
            return false;
        }
    } else {
        webrtc_sink->stun_server = NULL;
    }

    bool ok = sc_mutex_init(&webrtc_sink->mutex);
    if (!ok) {
        goto error_free_stun;
    }

    ok = sc_cond_init(&webrtc_sink->cond);
    if (!ok) {
        goto error_mutex_destroy;
    }

    webrtc_sink->video = video;
    webrtc_sink->audio = audio;

    sc_vecdeque_init(&webrtc_sink->video_queue);
    sc_vecdeque_init(&webrtc_sink->audio_queue);
    webrtc_sink->stopped = false;

    webrtc_sink->video_init = false;
    webrtc_sink->audio_init = false;

    webrtc_sink->webrtc_context = NULL;
    webrtc_sink->signaling_server = NULL;
    webrtc_sink->controller = controller;

    webrtc_sink->cbs = cbs;
    webrtc_sink->cbs_userdata = cbs_userdata;

    // 設置 packet_sink 操作接口
    if (video) {
        static const struct sc_packet_sink_ops video_ops = {
            .open = sc_webrtc_sink_video_packet_sink_open,
            .close = sc_webrtc_sink_video_packet_sink_close,
            .push = sc_webrtc_sink_video_packet_sink_push,
        };

        webrtc_sink->video_packet_sink.ops = &video_ops;
    }

    if (audio) {
        static const struct sc_packet_sink_ops audio_ops = {
            .open = sc_webrtc_sink_audio_packet_sink_open,
            .close = sc_webrtc_sink_audio_packet_sink_close,
            .push = sc_webrtc_sink_audio_packet_sink_push,
            .disable = sc_webrtc_sink_audio_packet_sink_disable,
        };

        webrtc_sink->audio_packet_sink.ops = &audio_ops;
    }

    LOGI("WebRTC sink initialized (port=%d, video=%d, audio=%d)",
         http_port, video, audio);

    return true;

error_mutex_destroy:
    sc_mutex_destroy(&webrtc_sink->mutex);
error_free_stun:
    free(webrtc_sink->stun_server);

    return false;
}

bool
sc_webrtc_sink_start(struct sc_webrtc_sink *webrtc_sink) {
    bool ok = sc_thread_create(&webrtc_sink->thread, run_webrtc_sink,
                               "scrcpy-webrtc", webrtc_sink);
    if (!ok) {
        LOGE("Could not start WebRTC sink thread");
        return false;
    }

    return true;
}

void
sc_webrtc_sink_stop(struct sc_webrtc_sink *webrtc_sink) {
    sc_mutex_lock(&webrtc_sink->mutex);
    webrtc_sink->stopped = true;
    sc_cond_signal(&webrtc_sink->cond);
    sc_mutex_unlock(&webrtc_sink->mutex);
}

void
sc_webrtc_sink_join(struct sc_webrtc_sink *webrtc_sink) {
    sc_thread_join(&webrtc_sink->thread, NULL);
}

void
sc_webrtc_sink_destroy(struct sc_webrtc_sink *webrtc_sink) {
    sc_cond_destroy(&webrtc_sink->cond);
    sc_mutex_destroy(&webrtc_sink->mutex);
    free(webrtc_sink->stun_server);
}
