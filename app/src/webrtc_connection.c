#include "webrtc_connection.h"

#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "util/log.h"

// TODO: 當安裝 libdatachannel 後啟用
#define HAVE_LIBDATACHANNEL

#ifdef HAVE_LIBDATACHANNEL
# include <rtc/rtc.h>
#endif

struct sc_webrtc_connection {
    const struct sc_webrtc_connection_callbacks *cbs;
    void *cbs_userdata;

    enum sc_webrtc_connection_state state;

#ifdef HAVE_LIBDATACHANNEL
    int pc_id;              // PeerConnection ID
    int video_track_id;     // 視頻軌道 ID
    int audio_track_id;     // 音頻軌道 ID
    int data_channel_id;    // Data Channel ID（用於觸控控制）

    // ICE gathering 同步機制
    char *pending_description;      // 待發送的 SDP（等待 ICE gathering 完成）
    char *pending_description_type; // 待發送的 SDP 類型（"offer" 或 "answer"）
    bool gathering_complete;        // ICE gathering 是否完成

    // Packetizer 設置狀態
    bool video_packetizer_set;      // 視頻 packetizer 是否已設置
    bool audio_packetizer_set;      // 音頻 packetizer 是否已設置

    // RTCP 和錯誤處理
    uint64_t last_packet_sent_time; // 最後發送封包的時間（毫秒）
    uint64_t packets_sent_count;    // 已發送封包計數

    // Track 開啟狀態
    bool video_track_open;  // 視頻 track 是否已開啟
    bool audio_track_open;  // 音頻 track 是否已開啟
#else
    void *pc_id;            // 佔位符
    void *video_track_id;
    void *audio_track_id;
#endif

    bool video_enabled;
    bool audio_enabled;
};

#ifdef HAVE_LIBDATACHANNEL
// ============================================================================
// libdatachannel 回調函數
// ============================================================================

static void
on_data_channel_open_callback(int dc, void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;
    (void) dc;

    LOGI("WebRTC: data channel opened (id=%d)", dc);

    // 觸發 data channel 開啟回調
    if (connection->cbs && connection->cbs->on_data_channel_open) {
        connection->cbs->on_data_channel_open(connection, connection->cbs_userdata);
    }
}

static void
on_data_channel_message_callback(int dc, const char *message, int size, void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;
    (void) dc;

    // libdatachannel 約定：
    // - size < 0: null-terminated 字符串
    // - size >= 0: 二進制數據，長度為 size
    size_t actual_size;
    if (size < 0) {
        // null-terminated 字符串，計算實際長度
        actual_size = strlen(message);
        LOGD("WebRTC: data channel text message received, length=%zu", actual_size);
    } else {
        // 二進制數據
        actual_size = (size_t)size;
        LOGD("WebRTC: data channel binary message received, size=%zu", actual_size);
    }

    // 觸發 data channel 消息回調
    if (connection->cbs && connection->cbs->on_data_channel_message) {
        connection->cbs->on_data_channel_message(connection,
                                                  (const uint8_t *)message,
                                                  actual_size,
                                                  connection->cbs_userdata);
    }
}

static void
on_data_channel_error_callback(int dc, const char *error, void *user_ptr) {
    (void) dc;
    (void) user_ptr;

    LOGE("WebRTC: data channel error: %s", error);
}

static void
on_description_callback(int pc, const char *sdp, const char *type,
                       void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;
    (void) pc;
    (void) sdp;

    LOGD("WebRTC: local description created, type=%s", type);

    // 只接受第一次調用，忽略重複調用
    if (connection->pending_description_type) {
        LOGD("WebRTC: ignoring duplicate description callback (already have %s, got %s)",
             connection->pending_description_type, type);
        return;
    }

    LOGD("WebRTC: storing description type, waiting for ICE gathering to complete");

    // 只保存類型，實際 SDP 在 ICE gathering 完成後獲取
    connection->pending_description_type = strdup(type);
    connection->gathering_complete = false;

    // 不立即調用回調，等待 ICE gathering 完成
}

static void
on_candidate_callback(int pc, const char *candidate, const char *mid,
                     void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;

    LOGD("WebRTC: ICE candidate generated, mid=%s", mid);

    if (connection->cbs && connection->cbs->on_ice_candidate) {
        connection->cbs->on_ice_candidate(connection, candidate, mid,
                                         connection->cbs_userdata);
    }
}

static void
on_track_callback(int pc, int tr, void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;
    (void) pc;

    // 獲取 track 的 mid 來確定是 video 還是 audio
    char mid[16];
    int result = rtcGetTrackMid(tr, mid, sizeof(mid));
    if (result < 0) {
        LOGE("Failed to get track mid: %d", result);
        return;
    }

    LOGI("WebRTC: track created by libdatachannel, id=%d, mid=%s", tr, mid);

    // 注意：我們現在使用主動創建 tracks（add_video_track/add_audio_track）
    // 所以正常情況下不應該進入這個 callback
    // 這個 callback 只用於記錄日誌，以防萬一
    LOGW("Track created by libdatachannel (not expected in proactive mode)");
}

static void
on_track_open_callback(int tr, void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;

    LOGI("WebRTC: track opened, id=%d", tr);

    // 標記 track 已開啟（packetizer 已在創建 track 時設置好）
    if (tr == connection->video_track_id) {
        connection->video_track_open = true;
        LOGI("Video track ready for media transmission (packetizer already configured)");
    } else if (tr == connection->audio_track_id) {
        connection->audio_track_open = true;
        LOGI("Audio track ready for media transmission (packetizer already configured)");
    } else {
        LOGW("Unknown track opened: id=%d", tr);
    }
}

static void
on_track_error_callback(int tr, const char *error, void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;

    LOGW("WebRTC: track error, id=%d, error=%s", tr, error);

    // 當視頻軌道發生錯誤時，請求新的 keyframe
    if (tr == connection->video_track_id) {
        LOGI("Video track error detected, requesting keyframe");
        if (connection->cbs && connection->cbs->on_keyframe_request) {
            connection->cbs->on_keyframe_request(connection,
                                                connection->cbs_userdata);
        }
    }
}

static void
on_track_message_callback(int tr, const char *message, int size, void *user_ptr) {
    (void) tr;
    (void) message;
    (void) size;
    (void) user_ptr;

    // 這個回調主要用於接收來自瀏覽器的消息
    // 在 WebRTC 媒體流中，RTCP 消息是自動處理的
    // 這裡我們記錄接收到消息的事件
    LOGV("WebRTC: received message on track %d, size=%d", tr, size);
}

static void
on_pli_handler_callback(int tr, void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;

    LOGI("WebRTC: received PLI (Picture Loss Indication) on track %d, requesting keyframe", tr);

    // 觸發 keyframe 請求回調
    if (connection->cbs && connection->cbs->on_keyframe_request) {
        connection->cbs->on_keyframe_request(connection, connection->cbs_userdata);
    }
}

static void
on_state_change_callback(int pc, rtcState state, void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;

    enum sc_webrtc_connection_state new_state;
    switch (state) {
        case RTC_NEW:
            new_state = SC_WEBRTC_STATE_NEW;
            break;
        case RTC_CONNECTING:
            new_state = SC_WEBRTC_STATE_CONNECTING;
            break;
        case RTC_CONNECTED:
            new_state = SC_WEBRTC_STATE_CONNECTED;
            break;
        case RTC_DISCONNECTED:
            new_state = SC_WEBRTC_STATE_DISCONNECTED;
            break;
        case RTC_FAILED:
            new_state = SC_WEBRTC_STATE_FAILED;
            break;
        case RTC_CLOSED:
            new_state = SC_WEBRTC_STATE_CLOSED;
            break;
        default:
            new_state = SC_WEBRTC_STATE_NEW;
    }

    connection->state = new_state;

    LOGI("WebRTC: connection state changed to %d", new_state);

    if (connection->cbs && connection->cbs->on_connection_state_change) {
        connection->cbs->on_connection_state_change(connection, new_state,
                                                   connection->cbs_userdata);
    }
}

static void
on_gathering_state_change_callback(int pc, rtcGatheringState state,
                                  void *user_ptr) {
    struct sc_webrtc_connection *connection = user_ptr;

    LOGD("WebRTC: ICE gathering state changed to %d", state);

    // 當 ICE gathering 完成時，獲取最終的 SDP（包含所有 ICE candidates）並發送
    if (state == RTC_GATHERING_COMPLETE && !connection->gathering_complete) {
        LOGI("WebRTC: ICE gathering complete, fetching final SDP with all candidates");
        connection->gathering_complete = true;

        // 確保有待發送的描述類型
        if (!connection->pending_description_type) {
            LOGW("No pending description type to send after ICE gathering");
            return;
        }

        // 獲取包含所有 ICE candidates 的最終 SDP
        char sdp_buffer[65536];  // 64KB 緩衝區
        int result = rtcGetLocalDescription(pc, sdp_buffer, sizeof(sdp_buffer));

        if (result < 0) {
            LOGE("Failed to get final local description: %d", result);
            return;
        }

        LOGD("Final SDP length: %d bytes (includes all ICE candidates)", result);

        // 使用我們保存的類型（answer 或 offer），而不是重新判斷
        const char *desc_type = connection->pending_description_type;

        // 日誌:檢查並計數 ICE candidates
        int candidate_count = 0;
        char *search_ptr = sdp_buffer;
        while ((search_ptr = strstr(search_ptr, "a=candidate:")) != NULL) {
            candidate_count++;
            search_ptr += 12; // 移動到下一個可能的位置
        }
        LOGI("SDP contains %d ICE candidates", candidate_count);

        // 日誌:顯示 SDP 的 m-line 部分以驗證順序
        const char *m_video = strstr(sdp_buffer, "m=video");
        const char *m_audio = strstr(sdp_buffer, "m=audio");
        if (m_video && m_audio) {
            LOGI("m-line order: %s comes first", (m_video < m_audio) ? "video" : "audio");
        } else {
            LOGW("Could not find both m=video and m=audio in SDP");
        }

        // 修復 BUNDLE/LS group 格式：移除 "video audio" 字符串
        // libdatachannel 會自動添加這些,但這會導致瀏覽器拒絕 SDP
        char *line_start = sdp_buffer;
        char *line_end;
        char fixed_sdp[65536];
        fixed_sdp[0] = '\0';

        while ((line_end = strchr(line_start, '\n')) != NULL) {
            size_t line_len = line_end - line_start + 1; // 包含 \n

            // 檢查是否是 BUNDLE 或 LS group 行
            if (strncmp(line_start, "a=group:BUNDLE ", 15) == 0 ||
                strncmp(line_start, "a=group:LS ", 11) == 0) {
                // 查找 " video audio" 並移除
                char *video_audio = strstr(line_start, " video audio");
                if (video_audio && video_audio < line_end) {
                    // 複製到 " video audio" 之前的部分
                    strncat(fixed_sdp, line_start, video_audio - line_start);
                    // 保留原始換行符格式 (檢查是否有 \r)
                    if (line_end > line_start && *(line_end - 1) == '\r') {
                        strcat(fixed_sdp, "\r\n");
                    } else {
                        strcat(fixed_sdp, "\n");
                    }
                    LOGD("Fixed BUNDLE/LS line: removed ' video audio'");
                } else {
                    // 沒有找到,直接複製整行
                    strncat(fixed_sdp, line_start, line_len);
                }
            } else {
                // 非 BUNDLE/LS 行,直接複製
                strncat(fixed_sdp, line_start, line_len);
            }

            line_start = line_end + 1;
        }

        // 複製最後一行 (如果沒有換行符結尾)
        if (*line_start != '\0') {
            strcat(fixed_sdp, line_start);
        }

        // 現在發送修復後的 SDP
        if (connection->cbs && connection->cbs->on_local_description) {
            LOGI("Sending %s with embedded ICE candidates to browser (BUNDLE fixed)", desc_type);

            connection->cbs->on_local_description(connection,
                                                 desc_type,
                                                 fixed_sdp,
                                                 connection->cbs_userdata);

            // 清理
            free(connection->pending_description);
            free(connection->pending_description_type);
            connection->pending_description = NULL;
            connection->pending_description_type = NULL;
        }
    }
}

#endif // HAVE_LIBDATACHANNEL

// ============================================================================
// 公開接口實現
// ============================================================================

bool
sc_webrtc_connection_init(struct sc_webrtc_connection **connection,
                          const struct sc_webrtc_connection_config *config,
                          const struct sc_webrtc_connection_callbacks *cbs,
                          void *cbs_userdata) {
    assert(connection);
    assert(config);

    struct sc_webrtc_connection *conn = malloc(sizeof(*conn));
    if (!conn) {
        LOG_OOM();
        return false;
    }

    conn->cbs = cbs;
    conn->cbs_userdata = cbs_userdata;
    conn->state = SC_WEBRTC_STATE_NEW;
    conn->video_enabled = config->video_enabled;
    conn->audio_enabled = config->audio_enabled;

#ifdef HAVE_LIBDATACHANNEL
    // 配置 STUN/TURN 服務器
    rtcConfiguration rtc_config;
    memset(&rtc_config, 0, sizeof(rtc_config));

    const char *ice_servers[2];
    int ice_server_count = 0;

    if (config->stun_server) {
        ice_servers[ice_server_count++] = config->stun_server;
    } else {
        // 默認使用 Google 的公共 STUN 服務器
        ice_servers[ice_server_count++] = "stun:stun.l.google.com:19302";
    }

    if (config->turn_server) {
        ice_servers[ice_server_count++] = config->turn_server;
    }

    rtc_config.iceServers = ice_servers;
    rtc_config.iceServersCount = ice_server_count;

    // 創建 PeerConnection
    int pc = rtcCreatePeerConnection(&rtc_config);
    if (pc < 0) {
        LOGE("Failed to create PeerConnection: %d", pc);
        free(conn);
        return false;
    }

    conn->pc_id = pc;
    conn->video_track_id = -1;
    conn->audio_track_id = -1;
    conn->data_channel_id = -1;

    // 初始化 ICE gathering 同步機制
    conn->pending_description = NULL;
    conn->pending_description_type = NULL;
    conn->gathering_complete = false;

    // 初始化 packetizer 設置狀態
    conn->video_packetizer_set = false;
    conn->audio_packetizer_set = false;

    // 初始化統計和錯誤處理
    conn->last_packet_sent_time = 0;
    conn->packets_sent_count = 0;

    // 初始化 track 開啟狀態
    conn->video_track_open = false;
    conn->audio_track_open = false;

    // 設置回調函數
    rtcSetUserPointer(pc, conn);
    rtcSetLocalDescriptionCallback(pc, on_description_callback);
    rtcSetLocalCandidateCallback(pc, on_candidate_callback);
    rtcSetStateChangeCallback(pc, on_state_change_callback);
    rtcSetGatheringStateChangeCallback(pc, on_gathering_state_change_callback);
    rtcSetTrackCallback(pc, on_track_callback);  // 當 track 創建時通知我們

    LOGI("WebRTC connection initialized with libdatachannel (pc=%d)", pc);
#else
    conn->pc_id = NULL;
    conn->video_track_id = NULL;
    conn->audio_track_id = NULL;

    LOGI("WebRTC connection initialized (placeholder mode)");
    LOGW("libdatachannel not available, WebRTC connection disabled");
    LOGW("To enable: install libdatachannel and recompile with "
         "HAVE_LIBDATACHANNEL");
#endif

    *connection = conn;
    return true;
}

bool
sc_webrtc_connection_create_offer(struct sc_webrtc_connection *connection) {
    assert(connection);

#ifdef HAVE_LIBDATACHANNEL
    LOGD("WebRTC: creating offer");

    int result = rtcSetLocalDescription(connection->pc_id, NULL);
    if (result < 0) {
        LOGE("Failed to create offer: %d", result);
        return false;
    }

    return true;
#else
    LOGD("WebRTC: would create offer (placeholder)");
    return true;
#endif
}

bool
sc_webrtc_connection_set_remote_description(
    struct sc_webrtc_connection *connection,
    const char *type,
    const char *sdp) {
    assert(connection);
    assert(type);
    assert(sdp);

#ifdef HAVE_LIBDATACHANNEL
    LOGD("WebRTC: setting remote description, type=%s", type);
    LOGD("SDP length: %zu", strlen(sdp));

    int result = rtcSetRemoteDescription(connection->pc_id, sdp, type);
    LOGD("rtcSetRemoteDescription returned: %d", result);

    if (result < 0) {
        LOGE("Failed to set remote description: %d", result);
        return false;
    }

    LOGD("Remote description set successfully");

    // 注意：libdatachannel 0.21.2 不需要顯式設置 packetizer
    // 視頻和音頻會自動處理 RTP 封包
    LOGI("SDP negotiation complete, tracks ready for media transmission");

    return true;
#else
    LOGD("WebRTC: would set remote description, type=%s (placeholder)", type);
    return true;
#endif
}

bool
sc_webrtc_connection_create_answer(struct sc_webrtc_connection *connection) {
    assert(connection);

#ifdef HAVE_LIBDATACHANNEL
    LOGD("WebRTC: creating answer");

    // rtcSetLocalDescription with NULL creates answer if remote desc is set
    int result = rtcSetLocalDescription(connection->pc_id, NULL);
    LOGD("rtcSetLocalDescription returned: %d", result);

    if (result < 0) {
        LOGE("Failed to create answer: %d", result);
        return false;
    }

    LOGD("Answer creation initiated successfully");
    return true;
#else
    LOGD("WebRTC: would create answer (placeholder)");
    return true;
#endif
}

bool
sc_webrtc_connection_add_ice_candidate(struct sc_webrtc_connection *connection,
                                       const char *candidate,
                                       const char *sdp_mid) {
    assert(connection);
    assert(candidate);

#ifdef HAVE_LIBDATACHANNEL
    LOGD("WebRTC: adding ICE candidate, mid=%s", sdp_mid);

    int result = rtcAddRemoteCandidate(connection->pc_id, candidate, sdp_mid);
    if (result < 0) {
        LOGE("Failed to add ICE candidate: %d", result);
        return false;
    }

    return true;
#else
    LOGD("WebRTC: would add ICE candidate (placeholder)");
    return true;
#endif
}

bool
sc_webrtc_connection_add_video_track(struct sc_webrtc_connection *connection,
                                     const char *track_id) {
    assert(connection);
    assert(track_id);

#ifdef HAVE_LIBDATACHANNEL
    LOGD("WebRTC: adding video track, id=%s", track_id);

    // 配置視頻軌道
    rtcTrackInit track_init;
    memset(&track_init, 0, sizeof(track_init));
    track_init.direction = RTC_DIRECTION_SENDONLY;
    track_init.codec = RTC_CODEC_H264;
    track_init.payloadType = 103;  // 使用瀏覽器 offer 中的 H.264 payload type
    track_init.ssrc = 0x12345678;  // SSRC (與 RTP packetizer 一致)
    track_init.name = "0";   // 使用 mid 值作為 track name
    track_init.msid = NULL;  // 讓 libdatachannel 完全自動生成 MSID（避免重複問題）
    // 設置 H.264 profile（這將在 SDP 中添加 fmtp 行）
    // 使用 Constrained Baseline Profile Level 3.1 (profile-level-id=42e01f)
    // 這是瀏覽器廣泛支持的 profile，與 libdatachannel 預設值一致
    track_init.profile = "profile-level-id=42e01f;packetization-mode=1;level-asymmetry-allowed=1";

    // 創建視頻軌道
    int track = rtcAddTrackEx(connection->pc_id, &track_init);
    if (track < 0) {
        LOGE("Failed to add video track: %d", track);
        return false;
    }

    connection->video_track_id = track;
    LOGI("WebRTC: video track added (id=%d)", track);

    // 設置 track 回調
    rtcSetOpenCallback(track, on_track_open_callback);
    rtcSetErrorCallback(track, on_track_error_callback);
    rtcSetMessageCallback(track, on_track_message_callback);

    // 設置用戶數據，以便回調函數可以訪問 connection
    rtcSetUserPointer(track, connection);

    // 第一步：設置 H.264 packetizer（必須在添加 RTCP 鏈之前）
    rtcPacketizerInit packetizer_config = {
        .ssrc = 0x12345678,                              // 與 track_init 一致
        .cname = "video-stream",                         // CNAME
        .payloadType = 103,                              // 與 track_init 一致
        .clockRate = 90000,                              // H.264 標準 clock rate
        .sequenceNumber = 0,                             // 初始序列號
        .timestamp = 0,                                  // 初始時間戳
        .maxFragmentSize = RTC_DEFAULT_MAX_FRAGMENT_SIZE,// MTU
        .nalSeparator = RTC_NAL_SEPARATOR_START_SEQUENCE,// Annex B (0x000001)
    };

    int packetizer_result = rtcSetH264Packetizer(track, &packetizer_config);
    if (packetizer_result < 0) {
        LOGE("Failed to set H.264 packetizer: %d", packetizer_result);
        return false;
    }
    LOGI("H.264 packetizer configured successfully");

    // 第二步：添加 RTCP Sender Report (SR) - 用於音視頻同步和統計
    int sr_result = rtcChainRtcpSrReporter(track);
    if (sr_result < 0) {
        LOGW("Failed to chain RTCP SR reporter: %d (continuing anyway)", sr_result);
    } else {
        LOGI("RTCP SR reporter chained successfully");
    }

    // 第三步：添加 RTCP NACK Responder - 用於丟包重傳
    int nack_result = rtcChainRtcpNackResponder(track, RTC_DEFAULT_MAX_STORED_PACKET_COUNT);
    if (nack_result < 0) {
        LOGW("Failed to chain RTCP NACK responder: %d (continuing anyway)", nack_result);
    } else {
        LOGI("RTCP NACK responder chained successfully (max packets: %d)",
             RTC_DEFAULT_MAX_STORED_PACKET_COUNT);
    }

    // 第四步：添加 PLI Handler - 處理接收端的關鍵幀請求
    int pli_result = rtcChainPliHandler(track, on_pli_handler_callback);
    if (pli_result < 0) {
        LOGW("Failed to chain PLI handler: %d (continuing anyway)", pli_result);
    } else {
        LOGI("PLI handler chained successfully");
    }

    connection->video_packetizer_set = true;
    LOGI("Video track created with full RTCP support (SR + NACK + PLI)");

    return true;
#else
    LOGD("WebRTC: would add video track, id=%s (placeholder)", track_id);
    return true;
#endif
}

bool
sc_webrtc_connection_add_audio_track(struct sc_webrtc_connection *connection,
                                     const char *track_id) {
    assert(connection);
    assert(track_id);

#ifdef HAVE_LIBDATACHANNEL
    LOGD("WebRTC: adding audio track, id=%s", track_id);

    // 配置音頻軌道
    rtcTrackInit track_init;
    memset(&track_init, 0, sizeof(track_init));
    track_init.direction = RTC_DIRECTION_SENDONLY;
    track_init.codec = RTC_CODEC_OPUS;
    track_init.payloadType = 111;  // 使用瀏覽器 offer 中的 Opus payload type
    track_init.ssrc = 0x87654321;  // SSRC (與 RTP packetizer 一致)
    track_init.name = "1";   // 使用 mid 值作為 track name
    track_init.msid = NULL;  // 讓 libdatachannel 自動生成 MSID

    // 創建音頻軌道
    int track = rtcAddTrackEx(connection->pc_id, &track_init);
    if (track < 0) {
        LOGE("Failed to add audio track: %d", track);
        return false;
    }

    connection->audio_track_id = track;
    LOGI("WebRTC: audio track added (id=%d)", track);

    // 設置 track 回調
    rtcSetOpenCallback(track, on_track_open_callback);
    rtcSetErrorCallback(track, on_track_error_callback);
    rtcSetMessageCallback(track, on_track_message_callback);

    // 設置用戶數據，以便回調函數可以訪問 connection
    rtcSetUserPointer(track, connection);

    // 第一步：設置 Opus packetizer（必須在添加 RTCP 鏈之前）
    rtcPacketizerInit audio_packetizer_config = {
        .ssrc = 0x87654321,                              // 與 track_init 一致
        .cname = "audio-stream",                         // CNAME
        .payloadType = 111,                              // 與 track_init 一致
        .clockRate = 48000,                              // Opus 標準 clock rate
        .sequenceNumber = 0,                             // 初始序列號
        .timestamp = 0,                                  // 初始時間戳
    };

    int audio_packetizer_result = rtcSetOpusPacketizer(track, &audio_packetizer_config);
    if (audio_packetizer_result < 0) {
        LOGE("Failed to set Opus packetizer: %d", audio_packetizer_result);
        return false;
    }
    LOGI("Opus packetizer configured successfully");

    // 第二步：添加 RTCP Sender Report (SR) - 用於音視頻同步和統計
    int sr_result = rtcChainRtcpSrReporter(track);
    if (sr_result < 0) {
        LOGW("Failed to chain RTCP SR reporter for audio: %d (continuing anyway)", sr_result);
    } else {
        LOGI("Audio RTCP SR reporter chained successfully");
    }

    // 第三步：添加 RTCP NACK Responder - 用於丟包重傳
    int nack_result = rtcChainRtcpNackResponder(track, RTC_DEFAULT_MAX_STORED_PACKET_COUNT);
    if (nack_result < 0) {
        LOGW("Failed to chain RTCP NACK responder for audio: %d (continuing anyway)", nack_result);
    } else {
        LOGI("Audio RTCP NACK responder chained successfully (max packets: %d)",
             RTC_DEFAULT_MAX_STORED_PACKET_COUNT);
    }

    connection->audio_packetizer_set = true;
    LOGI("Audio track created with full RTCP support (SR + NACK)");

    return true;
#else
    LOGD("WebRTC: would add audio track, id=%s (placeholder)", track_id);
    return true;
#endif
}

bool
sc_webrtc_connection_send_video_rtp(struct sc_webrtc_connection *connection,
                                    const uint8_t *data,
                                    size_t size,
                                    int64_t pts_us) {
    assert(connection);
    assert(data);

#ifdef HAVE_LIBDATACHANNEL
    if (connection->video_track_id < 0) {
        LOGV("Video track not initialized");
        return false;
    }

    // 檢查 track 是否已開啟
    if (!connection->video_track_open) {
        LOGV("Video track not open yet, skipping packet");
        return true;  // 靜默跳過，不報錯
    }

    // 設置 RTP timestamp
    // PTS 是微秒單位，需要轉換為秒，然後使用 libdatachannel API 轉換為 RTP timestamp
    double pts_seconds = pts_us / 1000000.0;
    uint32_t rtp_timestamp = 0;

    int ts_result = rtcTransformSecondsToTimestamp(connection->video_track_id,
                                                   pts_seconds,
                                                   &rtp_timestamp);
    if (ts_result >= 0) {
        // 設置 RTP timestamp
        rtcSetTrackRtpTimestamp(connection->video_track_id, rtp_timestamp);

        // 每 30 幀輸出一次日誌
        static int log_counter = 0;
        if (log_counter++ % 30 == 0) {
            LOGI("RTP timestamp set: pts_us=%" PRIi64 " -> rtp_ts=%u (%.3fs)",
                 pts_us, rtp_timestamp, pts_seconds);
        }
    } else {
        LOGW("Failed to convert PTS to RTP timestamp: %d", ts_result);
    }

    int result = rtcSendMessage(connection->video_track_id, (const char *)data,
                               (int)size);

    if (result < 0) {
        LOGW("Failed to send video RTP: %d (track may be closing)", result);
        return false;
    }

    // 更新統計
    connection->packets_sent_count++;

    return true;
#else
    // Placeholder
    return true;
#endif
}

bool
sc_webrtc_connection_send_audio_rtp(struct sc_webrtc_connection *connection,
                                    const uint8_t *data,
                                    size_t size) {
    assert(connection);
    assert(data);

#ifdef HAVE_LIBDATACHANNEL
    if (connection->audio_track_id < 0) {
        LOGV("Audio track not initialized");
        return false;
    }

    // 檢查 track 是否已開啟
    if (!connection->audio_track_open) {
        LOGV("Audio track not open yet, skipping packet");
        return true;  // 靜默跳過，不報錯
    }

    int result = rtcSendMessage(connection->audio_track_id, (const char *)data,
                               (int)size);
    if (result < 0) {
        LOGW("Failed to send audio RTP: %d (track may be closing)", result);
        return false;
    }

    return true;
#else
    // Placeholder
    return true;
#endif
}

bool
sc_webrtc_connection_create_data_channel(struct sc_webrtc_connection *connection,
                                         const char *label) {
    assert(connection);
    assert(label);

#ifdef HAVE_LIBDATACHANNEL
    LOGD("WebRTC: creating data channel, label=%s", label);

    // 創建 data channel
    int dc = rtcCreateDataChannel(connection->pc_id, label);
    if (dc < 0) {
        LOGE("Failed to create data channel: %d", dc);
        return false;
    }

    connection->data_channel_id = dc;
    LOGI("WebRTC: data channel created (id=%d, label=%s)", dc, label);

    // 設置 data channel 回調
    rtcSetUserPointer(dc, connection);
    rtcSetOpenCallback(dc, on_data_channel_open_callback);
    rtcSetMessageCallback(dc, on_data_channel_message_callback);
    rtcSetErrorCallback(dc, on_data_channel_error_callback);

    return true;
#else
    LOGD("WebRTC: would create data channel, label=%s (placeholder)", label);
    return true;
#endif
}

enum sc_webrtc_connection_state
sc_webrtc_connection_get_state(struct sc_webrtc_connection *connection) {
    assert(connection);
    return connection->state;
}

void
sc_webrtc_connection_destroy(struct sc_webrtc_connection *connection) {
    if (!connection) {
        return;
    }

#ifdef HAVE_LIBDATACHANNEL
    if (connection->pc_id >= 0) {
        rtcDeletePeerConnection(connection->pc_id);
        LOGD("WebRTC: PeerConnection destroyed (id=%d)", connection->pc_id);
    }

    // 清理 pending descriptions
    free(connection->pending_description);
    free(connection->pending_description_type);
#endif

    free(connection);
    LOGD("WebRTC connection destroyed");
}
