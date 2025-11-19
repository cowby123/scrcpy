#ifndef SC_WEBRTC_CONNECTION_H
#define SC_WEBRTC_CONNECTION_H

#include "common.h"

#include <stdbool.h>
#include <stdint.h>

/**
 * WebRTC 連接狀態
 */
enum sc_webrtc_connection_state {
    SC_WEBRTC_STATE_NEW,          // 剛創建
    SC_WEBRTC_STATE_CONNECTING,   // 正在連接
    SC_WEBRTC_STATE_CONNECTED,    // 已連接
    SC_WEBRTC_STATE_DISCONNECTED, // 已斷開
    SC_WEBRTC_STATE_FAILED,       // 連接失敗
    SC_WEBRTC_STATE_CLOSED,       // 已關閉
};

/**
 * WebRTC 連接對象（不透明指針）
 */
struct sc_webrtc_connection;

/**
 * WebRTC 連接回調函數
 */
struct sc_webrtc_connection_callbacks {
    /**
     * 當本地 SDP 描述生成時調用（offer 或 answer）
     *
     * @param connection WebRTC 連接實例
     * @param type "offer" 或 "answer"
     * @param sdp SDP 字符串
     * @param userdata 用戶數據
     */
    void (*on_local_description)(struct sc_webrtc_connection *connection,
                                 const char *type,
                                 const char *sdp,
                                 void *userdata);

    /**
     * 當生成 ICE candidate 時調用
     *
     * @param connection WebRTC 連接實例
     * @param candidate ICE candidate 字符串（JSON 格式）
     * @param sdp_mid SDP media ID
     * @param userdata 用戶數據
     */
    void (*on_ice_candidate)(struct sc_webrtc_connection *connection,
                            const char *candidate,
                            const char *sdp_mid,
                            void *userdata);

    /**
     * 當連接狀態變化時調用
     *
     * @param connection WebRTC 連接實例
     * @param state 新的連接狀態
     * @param userdata 用戶數據
     */
    void (*on_connection_state_change)(struct sc_webrtc_connection *connection,
                                      enum sc_webrtc_connection_state state,
                                      void *userdata);

    /**
     * 當數據通道打開時調用
     *
     * @param connection WebRTC 連接實例
     * @param userdata 用戶數據
     */
    void (*on_data_channel_open)(struct sc_webrtc_connection *connection,
                                void *userdata);

    /**
     * 當發生錯誤時調用
     *
     * @param connection WebRTC 連接實例
     * @param error 錯誤消息
     * @param userdata 用戶數據
     */
    void (*on_error)(struct sc_webrtc_connection *connection,
                    const char *error,
                    void *userdata);

    /**
     * 當收到 RTCP PLI/FIR 時調用（請求關鍵幀）
     *
     * @param connection WebRTC 連接實例
     * @param userdata 用戶數據
     */
    void (*on_keyframe_request)(struct sc_webrtc_connection *connection,
                               void *userdata);
};

/**
 * WebRTC 連接配置
 */
struct sc_webrtc_connection_config {
    const char *stun_server;  // STUN 服務器 URL (例如: "stun:stun.l.google.com:19302")
    const char *turn_server;  // TURN 服務器 URL (可選)
    const char *turn_username; // TURN 用戶名 (可選)
    const char *turn_password; // TURN 密碼 (可選)
    bool video_enabled;       // 是否啟用視頻
    bool audio_enabled;       // 是否啟用音頻
};

/**
 * 初始化 WebRTC 連接
 *
 * @param connection 指向連接指針的指針（輸出參數）
 * @param config 連接配置
 * @param cbs 回調函數結構
 * @param cbs_userdata 回調函數用戶數據
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_connection_init(struct sc_webrtc_connection **connection,
                          const struct sc_webrtc_connection_config *config,
                          const struct sc_webrtc_connection_callbacks *cbs,
                          void *cbs_userdata);

/**
 * 創建 offer（作為主動方發起連接）
 *
 * @param connection WebRTC 連接實例
 * @return 成功返回 true，失敗返回 false
 *         SDP offer 將通過 on_local_description 回調返回
 */
bool
sc_webrtc_connection_create_offer(struct sc_webrtc_connection *connection);

/**
 * 設置遠程 SDP 描述（處理接收到的 offer 或 answer）
 *
 * @param connection WebRTC 連接實例
 * @param type "offer" 或 "answer"
 * @param sdp SDP 字符串
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_connection_set_remote_description(
    struct sc_webrtc_connection *connection,
    const char *type,
    const char *sdp);

/**
 * 創建 answer（響應接收到的 offer）
 *
 * @param connection WebRTC 連接實例
 * @return 成功返回 true，失敗返回 false
 *         SDP answer 將通過 on_local_description 回調返回
 */
bool
sc_webrtc_connection_create_answer(struct sc_webrtc_connection *connection);

/**
 * 添加 ICE candidate（處理接收到的 ICE candidate）
 *
 * @param connection WebRTC 連接實例
 * @param candidate ICE candidate 字符串
 * @param sdp_mid SDP media ID
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_connection_add_ice_candidate(struct sc_webrtc_connection *connection,
                                       const char *candidate,
                                       const char *sdp_mid);

/**
 * 添加視頻軌道到連接
 *
 * @param connection WebRTC 連接實例
 * @param track_id 視頻軌道 ID（例如："video"）
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_connection_add_video_track(struct sc_webrtc_connection *connection,
                                     const char *track_id);

/**
 * 添加音頻軌道到連接
 *
 * @param connection WebRTC 連接實例
 * @param track_id 音頻軌道 ID（例如："audio"）
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_connection_add_audio_track(struct sc_webrtc_connection *connection,
                                     const char *track_id);

/**
 * 發送 RTP 數據包到視頻軌道
 *
 * @param connection WebRTC 連接實例
 * @param data RTP 數據包數據
 * @param size 數據包大小
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_connection_send_video_rtp(struct sc_webrtc_connection *connection,
                                    const uint8_t *data,
                                    size_t size);

/**
 * 發送 RTP 數據包到音頻軌道
 *
 * @param connection WebRTC 連接實例
 * @param data RTP 數據包數據
 * @param size 數據包大小
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_connection_send_audio_rtp(struct sc_webrtc_connection *connection,
                                    const uint8_t *data,
                                    size_t size);

/**
 * 獲取當前連接狀態
 *
 * @param connection WebRTC 連接實例
 * @return 當前連接狀態
 */
enum sc_webrtc_connection_state
sc_webrtc_connection_get_state(struct sc_webrtc_connection *connection);

/**
 * 關閉並銷毀 WebRTC 連接
 *
 * @param connection WebRTC 連接實例
 */
void
sc_webrtc_connection_destroy(struct sc_webrtc_connection *connection);

#endif
