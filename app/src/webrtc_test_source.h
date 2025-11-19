#ifndef SC_WEBRTC_TEST_SOURCE_H
#define SC_WEBRTC_TEST_SOURCE_H

#include "common.h"

#include <stdbool.h>
#include <stdint.h>
#include <libavcodec/avcodec.h>

/**
 * WebRTC 測試視頻源
 * 生成固定顏色的 H.264 視頻流用於測試
 */
struct sc_webrtc_test_source {
    AVCodecContext *codec_ctx;
    AVFrame *frame;
    AVPacket *packet;

    int width;
    int height;
    int framerate;

    uint64_t frame_count;
    int64_t pts;
};

/**
 * 初始化測試視頻源
 *
 * @param test_source 測試源實例
 * @param width 視頻寬度
 * @param height 視頻高度
 * @param framerate 幀率
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_test_source_init(struct sc_webrtc_test_source *test_source,
                           int width, int height, int framerate);

/**
 * 生成下一幀（藍色）並編碼成 H.264
 *
 * @param test_source 測試源實例
 * @param out_packet 輸出編碼後的 AVPacket（由調用者釋放）
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_webrtc_test_source_generate_frame(struct sc_webrtc_test_source *test_source,
                                     AVPacket **out_packet);

/**
 * 銷毀測試視頻源
 *
 * @param test_source 測試源實例
 */
void
sc_webrtc_test_source_destroy(struct sc_webrtc_test_source *test_source);

#endif
