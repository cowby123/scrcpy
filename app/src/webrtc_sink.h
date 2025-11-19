#ifndef SC_WEBRTC_SINK_H
#define SC_WEBRTC_SINK_H

#include "common.h"

#include <stdbool.h>
#include <stdint.h>
#include <libavcodec/packet.h>

#include "trait/packet_sink.h"
#include "util/thread.h"
#include "util/vecdeque.h"

struct sc_webrtc_sink_queue SC_VECDEQUE(AVPacket *);

struct sc_webrtc_sink {
    struct sc_packet_sink video_packet_sink;
    struct sc_packet_sink audio_packet_sink;

    /* WebRTC 運行狀態標記 */
    bool video;
    bool audio;

    /* WebRTC 配置參數 */
    uint16_t http_port;  // HTTP 信令服務器端口（默認 8080）
    char *stun_server;   // STUN 服務器地址（可選）

    /* 測試模式 */
    bool test_mode;      // 是否啟用測試模式（藍色畫面）
    void *test_source;   // 測試視頻源（sc_webrtc_test_source*）

    /* 線程管理 */
    sc_thread thread;
    sc_mutex mutex;
    sc_cond cond;
    bool stopped;

    /* 數據包隊列 */
    struct sc_webrtc_sink_queue video_queue;
    struct sc_webrtc_sink_queue audio_queue;

    /* 初始化狀態標記 */
    bool video_init;
    bool audio_init;

    /* WebRTC 連接狀態（後續階段實現）*/
    void *webrtc_context;  // 指向 WebRTC 上下文的不透明指針
    void *signaling_server;  // 指向信令服務器的不透明指針

    /* 回調接口 */
    const struct sc_webrtc_sink_callbacks *cbs;
    void *cbs_userdata;
};

struct sc_webrtc_sink_callbacks {
    /**
     * 當 WebRTC 連接成功建立時調用
     */
    void (*on_connected)(struct sc_webrtc_sink *webrtc_sink, void *userdata);

    /**
     * 當 WebRTC 連接斷開時調用
     */
    void (*on_disconnected)(struct sc_webrtc_sink *webrtc_sink, void *userdata);

    /**
     * 當發生錯誤時調用
     */
    void (*on_error)(struct sc_webrtc_sink *webrtc_sink, const char *error,
                     void *userdata);

    /**
     * 當需要請求 keyframe 時調用（通常在首次發送 RTP 包時）
     */
    void (*on_request_keyframe)(struct sc_webrtc_sink *webrtc_sink, void *userdata);
};

/**
 * 初始化 WebRTC sink
 *
 * @param webrtc_sink WebRTC sink 實例
 * @param http_port HTTP 信令服務器端口
 * @param stun_server STUN 服務器地址（可為 NULL）
 * @param video 是否啟用視頻傳輸
 * @param audio 是否啟用音頻傳輸
 * @param cbs 回調函數結構（可為 NULL）
 * @param cbs_userdata 回調函數用戶數據
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_sink_init(struct sc_webrtc_sink *webrtc_sink, uint16_t http_port,
                    const char *stun_server, bool video, bool audio,
                    const struct sc_webrtc_sink_callbacks *cbs,
                    void *cbs_userdata);

/**
 * 啟動 WebRTC sink 線程
 *
 * @param webrtc_sink WebRTC sink 實例
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_sink_start(struct sc_webrtc_sink *webrtc_sink);

/**
 * 停止 WebRTC sink
 *
 * @param webrtc_sink WebRTC sink 實例
 */
void
sc_webrtc_sink_stop(struct sc_webrtc_sink *webrtc_sink);

/**
 * 等待 WebRTC sink 線程結束
 *
 * @param webrtc_sink WebRTC sink 實例
 */
void
sc_webrtc_sink_join(struct sc_webrtc_sink *webrtc_sink);

/**
 * 銷毀 WebRTC sink 並釋放資源
 *
 * @param webrtc_sink WebRTC sink 實例
 */
void
sc_webrtc_sink_destroy(struct sc_webrtc_sink *webrtc_sink);

#endif
