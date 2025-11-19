#ifndef SC_WEBRTC_SIGNALING_H
#define SC_WEBRTC_SIGNALING_H

#include "common.h"

#include <stdbool.h>
#include <stdint.h>

/**
 * WebRTC 信令服務器
 *
 * 提供 HTTP 服務器來：
 * 1. 提供 HTML 播放器頁面
 * 2. 處理 WebRTC 信令（SDP offer/answer, ICE candidates）
 * 3. 管理客戶端連接
 */

struct sc_webrtc_signaling;

/**
 * 信令消息類型
 */
enum sc_signaling_message_type {
    SC_SIGNALING_OFFER,      // 客戶端發送的 SDP offer
    SC_SIGNALING_ANSWER,     // 服務器發送的 SDP answer
    SC_SIGNALING_ICE,        // ICE candidate
    SC_SIGNALING_ERROR,      // 錯誤消息
};

/**
 * 信令消息結構
 */
struct sc_signaling_message {
    enum sc_signaling_message_type type;
    const char *data;  // JSON 格式的數據
    size_t data_len;
};

/**
 * 信令服務器回調函數
 */
struct sc_webrtc_signaling_callbacks {
    /**
     * 當收到客戶端的 SDP offer 時調用（Answerer 模式）
     *
     * @param signaling 信令服務器實例
     * @param offer SDP offer 字符串
     * @param userdata 用戶數據
     * @return SDP answer 字符串（需要由調用者釋放）
     */
    char *(*on_offer)(struct sc_webrtc_signaling *signaling,
                      const char *offer, void *userdata);

    /**
     * 當需要獲取本地 SDP offer 時調用（Offerer 模式）
     *
     * @param signaling 信令服務器實例
     * @param userdata 用戶數據
     * @return SDP offer 字符串（需要由調用者釋放）
     */
    char *(*on_get_offer)(struct sc_webrtc_signaling *signaling,
                          void *userdata);

    /**
     * 當收到客戶端的 SDP answer 時調用（Offerer 模式）
     *
     * @param signaling 信令服務器實例
     * @param answer SDP answer 字符串
     * @param userdata 用戶數據
     */
    void (*on_answer)(struct sc_webrtc_signaling *signaling,
                      const char *answer, void *userdata);

    /**
     * 當收到客戶端的 ICE candidate 時調用
     *
     * @param signaling 信令服務器實例
     * @param candidate ICE candidate JSON 字符串
     * @param userdata 用戶數據
     */
    void (*on_ice_candidate)(struct sc_webrtc_signaling *signaling,
                            const char *candidate, void *userdata);

    /**
     * 當客戶端連接時調用
     *
     * @param signaling 信令服務器實例
     * @param userdata 用戶數據
     */
    void (*on_client_connected)(struct sc_webrtc_signaling *signaling,
                               void *userdata);

    /**
     * 當客戶端斷開時調用
     *
     * @param signaling 信令服務器實例
     * @param userdata 用戶數據
     */
    void (*on_client_disconnected)(struct sc_webrtc_signaling *signaling,
                                  void *userdata);
};

/**
 * 初始化信令服務器
 *
 * @param signaling 信令服務器實例
 * @param port HTTP 服務器端口
 * @param cbs 回調函數
 * @param cbs_userdata 回調函數用戶數據
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_signaling_init(struct sc_webrtc_signaling **signaling,
                         uint16_t port,
                         const struct sc_webrtc_signaling_callbacks *cbs,
                         void *cbs_userdata);

/**
 * 啟動信令服務器
 *
 * @param signaling 信令服務器實例
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_signaling_start(struct sc_webrtc_signaling *signaling);

/**
 * 停止信令服務器
 *
 * @param signaling 信令服務器實例
 */
void
sc_webrtc_signaling_stop(struct sc_webrtc_signaling *signaling);

/**
 * 銷毀信令服務器並釋放資源
 *
 * @param signaling 信令服務器實例
 */
void
sc_webrtc_signaling_destroy(struct sc_webrtc_signaling *signaling);

/**
 * 向客戶端發送 SDP answer
 *
 * @param signaling 信令服務器實例
 * @param answer SDP answer 字符串
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_signaling_send_answer(struct sc_webrtc_signaling *signaling,
                                const char *answer);

/**
 * 向客戶端發送 ICE candidate
 *
 * @param signaling 信令服務器實例
 * @param candidate ICE candidate 字符串
 * @param sdp_mid SDP media stream identification (通常是 "0", "1" 等)
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_signaling_send_ice_candidate(struct sc_webrtc_signaling *signaling,
                                       const char *candidate,
                                       const char *sdp_mid);

/**
 * 獲取 HTTP 服務器的 URL
 *
 * @param signaling 信令服務器實例
 * @param port 端口號
 * @return URL 字符串（例如："http://localhost:8080"）
 */
const char *
sc_webrtc_signaling_get_url(struct sc_webrtc_signaling *signaling,
                            uint16_t port);

#endif
