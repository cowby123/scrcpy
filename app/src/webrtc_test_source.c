#include "webrtc_test_source.h"

#include <assert.h>
#include <string.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>

#include "util/log.h"

bool
sc_webrtc_test_source_init(struct sc_webrtc_test_source *test_source,
                           int width, int height, int framerate) {
    assert(test_source);

    test_source->width = width;
    test_source->height = height;
    test_source->framerate = framerate;
    test_source->frame_count = 0;
    test_source->pts = 0;

    // 尋找 H.264 編碼器
    const AVCodec *codec = avcodec_find_encoder(AV_CODEC_ID_H264);
    if (!codec) {
        LOGE("H.264 encoder not found");
        return false;
    }

    // 創建編碼器上下文
    test_source->codec_ctx = avcodec_alloc_context3(codec);
    if (!test_source->codec_ctx) {
        LOG_OOM();
        return false;
    }

    // 配置編碼器
    test_source->codec_ctx->width = width;
    test_source->codec_ctx->height = height;
    test_source->codec_ctx->time_base = (AVRational){1, framerate};
    test_source->codec_ctx->framerate = (AVRational){framerate, 1};
    test_source->codec_ctx->pix_fmt = AV_PIX_FMT_YUV420P;

    // H.264 編碼參數
    test_source->codec_ctx->bit_rate = 1000000;  // 1 Mbps
    test_source->codec_ctx->gop_size = framerate;  // 每秒一個 I 幀
    test_source->codec_ctx->max_b_frames = 0;  // 不使用 B 幀，簡化實現

    // 設置 H.264 profile (Constrained Baseline)
    av_opt_set(test_source->codec_ctx->priv_data, "profile", "baseline", 0);
    av_opt_set(test_source->codec_ctx->priv_data, "preset", "ultrafast", 0);
    av_opt_set(test_source->codec_ctx->priv_data, "tune", "zerolatency", 0);

    // 重要：在每個關鍵幀前重複發送 SPS/PPS
    // 這對於 WebRTC 至關重要，因為：
    // 1. 瀏覽器需要 SPS/PPS 才能初始化 H.264 解碼器
    // 2. 如果連接中斷重連，需要重新發送 SPS/PPS
    // 3. 確保每個關鍵幀都是獨立可解碼的
    av_opt_set(test_source->codec_ctx->priv_data, "repeat-headers", "1", 0);

    // 強制每幀都是關鍵幀（測試用途）
    // 這樣可以確保每個包都包含 SPS/PPS 和 IDR 幀
    test_source->codec_ctx->gop_size = 1;

    // 打開編碼器
    int ret = avcodec_open2(test_source->codec_ctx, codec, NULL);
    if (ret < 0) {
        char errbuf[128];
        av_strerror(ret, errbuf, sizeof(errbuf));
        LOGE("Failed to open H.264 encoder: %s", errbuf);
        avcodec_free_context(&test_source->codec_ctx);
        return false;
    }

    // 分配視頻幀
    test_source->frame = av_frame_alloc();
    if (!test_source->frame) {
        LOG_OOM();
        avcodec_free_context(&test_source->codec_ctx);
        return false;
    }

    test_source->frame->format = test_source->codec_ctx->pix_fmt;
    test_source->frame->width = width;
    test_source->frame->height = height;

    ret = av_frame_get_buffer(test_source->frame, 0);
    if (ret < 0) {
        LOGE("Failed to allocate frame buffer");
        av_frame_free(&test_source->frame);
        avcodec_free_context(&test_source->codec_ctx);
        return false;
    }

    // 分配 packet
    test_source->packet = av_packet_alloc();
    if (!test_source->packet) {
        LOG_OOM();
        av_frame_free(&test_source->frame);
        avcodec_free_context(&test_source->codec_ctx);
        return false;
    }

    LOGI("WebRTC test source initialized: %dx%d @ %d fps", width, height, framerate);
    return true;
}

/**
 * 填充藍色到 YUV420P 幀
 * 藍色在 YUV 中的值約為: Y=29, U=255, V=107
 */
static void
fill_blue_frame(AVFrame *frame) {
    // Y 平面（亮度）- 設置為深藍色的亮度值
    memset(frame->data[0], 29, frame->linesize[0] * frame->height);

    // U 平面（色度 Cb）- 設置為藍色的 U 分量
    memset(frame->data[1], 255, frame->linesize[1] * (frame->height / 2));

    // V 平面（色度 Cr）- 設置為藍色的 V 分量
    memset(frame->data[2], 107, frame->linesize[2] * (frame->height / 2));
}

bool
sc_webrtc_test_source_generate_frame(struct sc_webrtc_test_source *test_source,
                                     AVPacket **out_packet) {
    assert(test_source);
    assert(out_packet);

    // 確保幀可寫
    int ret = av_frame_make_writable(test_source->frame);
    if (ret < 0) {
        LOGE("Failed to make frame writable");
        return false;
    }

    // 填充藍色
    fill_blue_frame(test_source->frame);

    // 設置 PTS
    test_source->frame->pts = test_source->pts;
    test_source->pts += 1;

    // 編碼幀
    ret = avcodec_send_frame(test_source->codec_ctx, test_source->frame);
    if (ret < 0) {
        char errbuf[128];
        av_strerror(ret, errbuf, sizeof(errbuf));
        LOGE("Failed to send frame to encoder: %s", errbuf);
        return false;
    }

    // 接收編碼後的包
    ret = avcodec_receive_packet(test_source->codec_ctx, test_source->packet);
    if (ret == AVERROR(EAGAIN)) {
        // 需要更多幀才能輸出
        return false;
    } else if (ret < 0) {
        char errbuf[128];
        av_strerror(ret, errbuf, sizeof(errbuf));
        LOGE("Failed to receive packet from encoder: %s", errbuf);
        return false;
    }

    // 複製 packet 給調用者
    AVPacket *pkt = av_packet_alloc();
    if (!pkt) {
        LOG_OOM();
        av_packet_unref(test_source->packet);
        return false;
    }

    ret = av_packet_ref(pkt, test_source->packet);
    if (ret < 0) {
        LOGE("Failed to reference packet");
        av_packet_free(&pkt);
        av_packet_unref(test_source->packet);
        return false;
    }

    av_packet_unref(test_source->packet);

    *out_packet = pkt;
    test_source->frame_count++;

    if (test_source->frame_count % test_source->framerate == 0) {
        LOGD("Generated %llu blue test frames",
             (unsigned long long)test_source->frame_count);
    }

    return true;
}

void
sc_webrtc_test_source_destroy(struct sc_webrtc_test_source *test_source) {
    if (!test_source) {
        return;
    }

    if (test_source->packet) {
        av_packet_free(&test_source->packet);
    }

    if (test_source->frame) {
        av_frame_free(&test_source->frame);
    }

    if (test_source->codec_ctx) {
        avcodec_free_context(&test_source->codec_ctx);
    }

    LOGI("WebRTC test source destroyed, generated %llu frames",
         (unsigned long long)test_source->frame_count);
}
