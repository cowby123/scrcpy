#include "webrtc_signaling.h"

#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "util/log.h"
#include "util/thread.h"
#include "webrtc/html_player.h"

// TODO: 當安裝 libmicrohttpd 後啟用
#define HAVE_LIBMICROHTTPD

#ifdef HAVE_LIBMICROHTTPD
# include <microhttpd.h>
#endif

// ICE candidate 隊列節點
struct ice_candidate_node {
    char *candidate_json;
    struct ice_candidate_node *next;
};

struct sc_webrtc_signaling {
    uint16_t port;
    const struct sc_webrtc_signaling_callbacks *cbs;
    void *cbs_userdata;

#ifdef HAVE_LIBMICROHTTPD
    struct MHD_Daemon *http_daemon;
#else
    void *http_daemon;  // 佔位符
#endif

    bool running;
    char *pending_answer;
    char *pending_ice_candidate;  // 保留以兼容舊代碼

    // ICE candidate 隊列（用於發送到瀏覽器）
    struct ice_candidate_node *ice_queue_head;
    struct ice_candidate_node *ice_queue_tail;
    sc_mutex ice_queue_mutex;
};

#ifdef HAVE_LIBMICROHTTPD
// POST 請求數據緩衝
struct connection_info {
    char *data;
    size_t size;
};
#endif

#ifdef HAVE_LIBMICROHTTPD
/**
 * 請求完成時的清理回調
 */
static void
request_completed(void *cls, struct MHD_Connection *connection,
                 void **con_cls, enum MHD_RequestTerminationCode toe) {
    (void) cls;
    (void) connection;
    (void) toe;

    struct connection_info *con_info = *con_cls;
    if (con_info) {
        free(con_info->data);
        free(con_info);
        *con_cls = NULL;
    }
}

/**
 * HTTP 請求處理函數
 */
static enum MHD_Result
http_request_handler(void *cls, struct MHD_Connection *connection,
                    const char *url, const char *method,
                    const char *version, const char *upload_data,
                    size_t *upload_data_size, void **con_cls) {
    struct sc_webrtc_signaling *signaling = cls;
    (void) version;

    // GET /  - 返回 HTML 播放器
    if (strcmp(url, "/") == 0 && strcmp(method, "GET") == 0) {
        LOGD("HTTP: GET /");
        struct MHD_Response *response =
            MHD_create_response_from_buffer(strlen(sc_webrtc_html_player),
                                          (void *) sc_webrtc_html_player,
                                          MHD_RESPMEM_PERSISTENT);
        MHD_add_response_header(response, "Content-Type", "text/html; charset=utf-8");
        MHD_add_response_header(response, "Cache-Control", "no-cache");
        enum MHD_Result ret = MHD_queue_response(connection, MHD_HTTP_OK,
                                                response);
        MHD_destroy_response(response);
        return ret;
    }

    // GET /ice - 返回服務器生成的 ICE candidates（JSON 數組）
    if (strcmp(url, "/ice") == 0 && strcmp(method, "GET") == 0) {
        LOGD("HTTP: GET /ice");

        // 構建 JSON 數組
        char *json = NULL;
        size_t json_len = 0;

        sc_mutex_lock(&signaling->ice_queue_mutex);

        if (!signaling->ice_queue_head) {
            // 隊列為空，返回空數組
            json = strdup("[]");
            json_len = 2;
        } else {
            // 計算需要的緩衝區大小並構建 JSON 數組
            // 格式: [{"candidate":"...","sdpMid":"...","sdpMLineIndex":0},...]
            size_t buffer_size = 4096; // 初始大小
            json = malloc(buffer_size);
            if (!json) {
                sc_mutex_unlock(&signaling->ice_queue_mutex);
                const char *error = "{\"error\":\"Out of memory\"}";
                struct MHD_Response *response =
                    MHD_create_response_from_buffer(strlen(error),
                                                  (void *) error,
                                                  MHD_RESPMEM_PERSISTENT);
                MHD_add_response_header(response, "Content-Type",
                                      "application/json");
                MHD_add_response_header(response, "Access-Control-Allow-Origin",
                                      "*");
                enum MHD_Result ret = MHD_queue_response(connection,
                                                        MHD_HTTP_INTERNAL_SERVER_ERROR,
                                                        response);
                MHD_destroy_response(response);
                return ret;
            }

            strcpy(json, "[");
            json_len = 1;

            struct ice_candidate_node *node = signaling->ice_queue_head;
            bool first = true;
            while (node) {
                // 添加逗號分隔符（除了第一個）
                if (!first) {
                    if (json_len + 1 >= buffer_size) {
                        buffer_size *= 2;
                        char *new_json = realloc(json, buffer_size);
                        if (!new_json) {
                            free(json);
                            sc_mutex_unlock(&signaling->ice_queue_mutex);
                            const char *error = "{\"error\":\"Out of memory\"}";
                            struct MHD_Response *response =
                                MHD_create_response_from_buffer(strlen(error),
                                                              (void *) error,
                                                              MHD_RESPMEM_PERSISTENT);
                            MHD_add_response_header(response, "Content-Type",
                                                  "application/json");
                            MHD_add_response_header(response, "Access-Control-Allow-Origin",
                                                  "*");
                            enum MHD_Result ret = MHD_queue_response(connection,
                                                                    MHD_HTTP_INTERNAL_SERVER_ERROR,
                                                                    response);
                            MHD_destroy_response(response);
                            return ret;
                        }
                        json = new_json;
                    }
                    strcat(json, ",");
                    json_len++;
                }
                first = false;

                // 添加當前 candidate（已經是 JSON 格式）
                size_t candidate_len = strlen(node->candidate_json);
                if (json_len + candidate_len + 1 >= buffer_size) {
                    buffer_size = json_len + candidate_len + 128;
                    char *new_json = realloc(json, buffer_size);
                    if (!new_json) {
                        free(json);
                        sc_mutex_unlock(&signaling->ice_queue_mutex);
                        const char *error = "{\"error\":\"Out of memory\"}";
                        struct MHD_Response *response =
                            MHD_create_response_from_buffer(strlen(error),
                                                          (void *) error,
                                                          MHD_RESPMEM_PERSISTENT);
                        MHD_add_response_header(response, "Content-Type",
                                              "application/json");
                        MHD_add_response_header(response, "Access-Control-Allow-Origin",
                                              "*");
                        enum MHD_Result ret = MHD_queue_response(connection,
                                                                MHD_HTTP_INTERNAL_SERVER_ERROR,
                                                                response);
                        MHD_destroy_response(response);
                        return ret;
                    }
                    json = new_json;
                }
                strcat(json, node->candidate_json);
                json_len += candidate_len;

                node = node->next;
            }

            // 添加結束括號
            if (json_len + 2 >= buffer_size) {
                char *new_json = realloc(json, json_len + 3);
                if (new_json) {
                    json = new_json;
                }
            }
            strcat(json, "]");
            json_len++;

            // 清空隊列
            node = signaling->ice_queue_head;
            while (node) {
                struct ice_candidate_node *next = node->next;
                free(node->candidate_json);
                free(node);
                node = next;
            }
            signaling->ice_queue_head = NULL;
            signaling->ice_queue_tail = NULL;

            LOGI("=== SENDING %d ICE CANDIDATES TO BROWSER ===",
                 first ? 0 : 1);  // 簡單計數
        }

        sc_mutex_unlock(&signaling->ice_queue_mutex);

        struct MHD_Response *response =
            MHD_create_response_from_buffer(json_len, json,
                                          MHD_RESPMEM_MUST_FREE);
        MHD_add_response_header(response, "Content-Type",
                              "application/json");
        MHD_add_response_header(response, "Access-Control-Allow-Origin",
                              "*");
        MHD_add_response_header(response, "Cache-Control", "no-cache");
        enum MHD_Result ret = MHD_queue_response(connection,
                                                MHD_HTTP_OK, response);
        MHD_destroy_response(response);
        return ret;
    }

    // GET /offer - 返回 SDP offer（Offerer 模式）
    if (strcmp(url, "/offer") == 0 && strcmp(method, "GET") == 0) {
        LOGD("HTTP: GET /offer");
        if (signaling->cbs && signaling->cbs->on_get_offer) {
            char *offer = signaling->cbs->on_get_offer(signaling,
                                                       signaling->cbs_userdata);

            if (offer) {
                LOGI("=== SENDING OFFER SDP TO BROWSER ===");
                LOGI("%s", offer);
                LOGI("=== END OF OFFER SDP ===");

                struct MHD_Response *response =
                    MHD_create_response_from_buffer(strlen(offer),
                                                  (void *) offer,
                                                  MHD_RESPMEM_MUST_COPY);
                MHD_add_response_header(response, "Content-Type",
                                      "application/json");
                MHD_add_response_header(response, "Access-Control-Allow-Origin",
                                      "*");
                MHD_add_response_header(response, "Cache-Control", "no-cache");
                enum MHD_Result ret = MHD_queue_response(connection,
                                                        MHD_HTTP_OK, response);
                MHD_destroy_response(response);
                free(offer);
                return ret;
            } else {
                LOGE("Failed to get offer from callback");
                const char *error = "{\"error\":\"Failed to create offer\"}";
                struct MHD_Response *response =
                    MHD_create_response_from_buffer(strlen(error),
                                                  (void *) error,
                                                  MHD_RESPMEM_PERSISTENT);
                MHD_add_response_header(response, "Content-Type",
                                      "application/json");
                MHD_add_response_header(response, "Access-Control-Allow-Origin",
                                      "*");
                enum MHD_Result ret = MHD_queue_response(connection,
                                                        MHD_HTTP_INTERNAL_SERVER_ERROR,
                                                        response);
                MHD_destroy_response(response);
                return ret;
            }
        }
    }

    // 處理 POST 請求
    if (strcmp(method, "POST") == 0) {
        struct connection_info *con_info = *con_cls;

        // 第一次調用，創建 connection_info
        if (!con_info) {
            con_info = malloc(sizeof(struct connection_info));
            if (!con_info) {
                return MHD_NO;
            }
            con_info->data = NULL;
            con_info->size = 0;
            *con_cls = con_info;
            return MHD_YES;
        }

        // 接收 POST 數據
        if (*upload_data_size > 0) {
            char *new_data = realloc(con_info->data,
                                    con_info->size + *upload_data_size + 1);
            if (!new_data) {
                return MHD_NO;
            }
            memcpy(new_data + con_info->size, upload_data, *upload_data_size);
            con_info->size += *upload_data_size;
            new_data[con_info->size] = '\0';
            con_info->data = new_data;
            *upload_data_size = 0;
            return MHD_YES;
        }

        // POST 數據接收完成，處理請求
        LOGD("HTTP: POST %s (size=%zu)", url, con_info->size);

        // POST /answer - 接收 SDP answer（Offerer 模式）
        if (strcmp(url, "/answer") == 0) {
            if (signaling->cbs && signaling->cbs->on_answer && con_info->data) {
                // 打印完整的接收到的 answer SDP
                LOGI("=== RECEIVED ANSWER SDP FROM BROWSER ===");
                LOGI("%s", con_info->data);
                LOGI("=== END OF ANSWER SDP ===");

                signaling->cbs->on_answer(signaling, con_info->data,
                                         signaling->cbs_userdata);

                const char *ok = "{\"status\":\"ok\"}";
                struct MHD_Response *response =
                    MHD_create_response_from_buffer(strlen(ok), (void *) ok,
                                                  MHD_RESPMEM_PERSISTENT);
                MHD_add_response_header(response, "Content-Type",
                                      "application/json");
                MHD_add_response_header(response, "Access-Control-Allow-Origin",
                                      "*");
                enum MHD_Result ret = MHD_queue_response(connection,
                                                        MHD_HTTP_OK, response);
                MHD_destroy_response(response);
                return ret;
            }
        }

        // POST /ice - 接收 ICE candidate
        if (strcmp(url, "/ice") == 0) {
            LOGI("=== RECEIVED ICE CANDIDATE FROM BROWSER ===");

            // 檢查條件
            if (!signaling->cbs) {
                LOGE("ERROR: signaling->cbs is NULL!");
            } else if (!signaling->cbs->on_ice_candidate) {
                LOGE("ERROR: on_ice_candidate callback is NULL!");
            } else if (!con_info->data) {
                LOGE("ERROR: con_info->data is NULL!");
            } else {
                // 所有條件都滿足，打印收到的數據並調用回調
                LOGI("ICE candidate data: %s", con_info->data);

                signaling->cbs->on_ice_candidate(signaling, con_info->data,
                                                signaling->cbs_userdata);
            }

            const char *ok = "{\"status\":\"ok\"}";
            struct MHD_Response *response =
                MHD_create_response_from_buffer(strlen(ok), (void *) ok,
                                              MHD_RESPMEM_PERSISTENT);
            MHD_add_response_header(response, "Content-Type",
                                  "application/json");
            MHD_add_response_header(response, "Access-Control-Allow-Origin",
                                  "*");
            enum MHD_Result ret = MHD_queue_response(connection,
                                                    MHD_HTTP_OK, response);
            MHD_destroy_response(response);
            return ret;
        }
    }

    // 處理 OPTIONS 請求 (CORS preflight)
    if (strcmp(method, "OPTIONS") == 0) {
        struct MHD_Response *response =
            MHD_create_response_from_buffer(0, "", MHD_RESPMEM_PERSISTENT);
        MHD_add_response_header(response, "Access-Control-Allow-Origin", "*");
        MHD_add_response_header(response, "Access-Control-Allow-Methods",
                              "POST, GET, OPTIONS");
        MHD_add_response_header(response, "Access-Control-Allow-Headers",
                              "Content-Type");
        enum MHD_Result ret = MHD_queue_response(connection, MHD_HTTP_OK,
                                                response);
        MHD_destroy_response(response);
        return ret;
    }

    // 404 for other requests
    const char *not_found = "Not Found";
    struct MHD_Response *response =
        MHD_create_response_from_buffer(strlen(not_found), (void *) not_found,
                                      MHD_RESPMEM_PERSISTENT);
    enum MHD_Result ret = MHD_queue_response(connection, MHD_HTTP_NOT_FOUND,
                                            response);
    MHD_destroy_response(response);
    return ret;
}
#endif

bool
sc_webrtc_signaling_init(struct sc_webrtc_signaling **signaling,
                         uint16_t port,
                         const struct sc_webrtc_signaling_callbacks *cbs,
                         void *cbs_userdata) {
    assert(signaling);

    struct sc_webrtc_signaling *s = malloc(sizeof(*s));
    if (!s) {
        LOG_OOM();
        return false;
    }

    s->port = port;
    s->cbs = cbs;
    s->cbs_userdata = cbs_userdata;
    s->http_daemon = NULL;
    s->running = false;
    s->pending_answer = NULL;
    s->pending_ice_candidate = NULL;

    // 初始化 ICE candidate 隊列
    s->ice_queue_head = NULL;
    s->ice_queue_tail = NULL;
    sc_mutex_init(&s->ice_queue_mutex);

    *signaling = s;

    LOGI("WebRTC signaling initialized (port=%d)", port);

#ifndef HAVE_LIBMICROHTTPD
    LOGW("libmicrohttpd not available, signaling server disabled");
    LOGW("To enable: install libmicrohttpd and recompile with HAVE_LIBMICROHTTPD");
#endif

    return true;
}

bool
sc_webrtc_signaling_start(struct sc_webrtc_signaling *signaling) {
    assert(signaling);

#ifdef HAVE_LIBMICROHTTPD
    LOGI("Starting HTTP signaling server on port %d", signaling->port);

    signaling->http_daemon = MHD_start_daemon(
        MHD_USE_SELECT_INTERNALLY | MHD_USE_DEBUG,
        signaling->port,
        NULL, NULL,  // accept policy callback
        &http_request_handler, signaling,
        MHD_OPTION_NOTIFY_COMPLETED, &request_completed, NULL,
        MHD_OPTION_END
    );

    if (!signaling->http_daemon) {
        LOGE("Failed to start HTTP server on port %d", signaling->port);
        return false;
    }

    signaling->running = true;
    LOGI("HTTP signaling server started at http://localhost:%d",
         signaling->port);

    return true;
#else
    LOGI("Signaling server: placeholder mode (libmicrohttpd not available)");
    signaling->running = true;
    return true;
#endif
}

void
sc_webrtc_signaling_stop(struct sc_webrtc_signaling *signaling) {
    if (!signaling || !signaling->running) {
        return;
    }

#ifdef HAVE_LIBMICROHTTPD
    if (signaling->http_daemon) {
        MHD_stop_daemon(signaling->http_daemon);
        signaling->http_daemon = NULL;
    }
#endif

    signaling->running = false;
    LOGI("WebRTC signaling server stopped");
}

void
sc_webrtc_signaling_destroy(struct sc_webrtc_signaling *signaling) {
    if (!signaling) {
        return;
    }

    free(signaling->pending_answer);
    free(signaling->pending_ice_candidate);

    // 清理 ICE candidate 隊列
    struct ice_candidate_node *node = signaling->ice_queue_head;
    while (node) {
        struct ice_candidate_node *next = node->next;
        free(node->candidate_json);
        free(node);
        node = next;
    }

    sc_mutex_destroy(&signaling->ice_queue_mutex);
    free(signaling);
}

bool
sc_webrtc_signaling_send_answer(struct sc_webrtc_signaling *signaling,
                                const char *answer) {
    assert(signaling);
    assert(answer);

    // TODO: 實現通過 HTTP response 或 WebSocket 發送 answer
    LOGD("Signaling: would send answer (len=%zu)", strlen(answer));

    free(signaling->pending_answer);
    signaling->pending_answer = strdup(answer);

    return signaling->pending_answer != NULL;
}

bool
sc_webrtc_signaling_send_ice_candidate(struct sc_webrtc_signaling *signaling,
                                       const char *candidate,
                                       const char *sdp_mid) {
    assert(signaling);
    assert(candidate);
    assert(sdp_mid);

    LOGD("Signaling: queuing ICE candidate for browser (mid=%s, len=%zu)",
         sdp_mid, strlen(candidate));

    // 構建完整的 RTCIceCandidateInit JSON 對象
    // 格式: {"candidate":"...","sdpMid":"...","sdpMLineIndex":0}
    // 注意：sdpMLineIndex 通常與 sdpMid 對應（"0"->0, "1"->1）
    int sdp_mline_index = atoi(sdp_mid);

    // 計算所需緩衝區大小（candidate + sdpMid + JSON 格式字符）
    size_t json_size = strlen(candidate) + strlen(sdp_mid) + 128;
    char *candidate_json = malloc(json_size);
    if (!candidate_json) {
        LOGE("Failed to allocate ICE candidate JSON");
        return false;
    }

    // 構建 JSON 對象
    int written = snprintf(candidate_json, json_size,
                          "{\"candidate\":\"%s\",\"sdpMid\":\"%s\",\"sdpMLineIndex\":%d}",
                          candidate, sdp_mid, sdp_mline_index);

    if (written < 0 || (size_t)written >= json_size) {
        LOGE("Failed to format ICE candidate JSON");
        free(candidate_json);
        return false;
    }

    // 創建新節點
    struct ice_candidate_node *node = malloc(sizeof(*node));
    if (!node) {
        LOGE("Failed to allocate ICE candidate node");
        free(candidate_json);
        return false;
    }

    node->candidate_json = candidate_json;
    node->next = NULL;

    // 添加到隊列尾部（線程安全）
    sc_mutex_lock(&signaling->ice_queue_mutex);
    if (signaling->ice_queue_tail) {
        signaling->ice_queue_tail->next = node;
        signaling->ice_queue_tail = node;
    } else {
        signaling->ice_queue_head = node;
        signaling->ice_queue_tail = node;
    }
    sc_mutex_unlock(&signaling->ice_queue_mutex);

    LOGD("ICE candidate queued successfully: %s", candidate_json);
    return true;
}

const char *
sc_webrtc_signaling_get_url(struct sc_webrtc_signaling *signaling,
                            uint16_t port) {
    static char url[256];
    snprintf(url, sizeof(url), "http://localhost:%d", port);
    return url;
}
