#ifndef SC_RTP_PACKETIZER_H
#define SC_RTP_PACKETIZER_H

#include "common.h"

#include <stdbool.h>
#include <stdint.h>
#include <libavcodec/packet.h>

/**
 * RTP H.264 打包器
 *
 * 根據 RFC 6184 標準將 H.264 NAL units 打包成 RTP packets
 */

/**
 * H.264 NAL unit 類型
 */
enum sc_h264_nal_type {
    SC_H264_NAL_UNSPECIFIED = 0,
    SC_H264_NAL_SLICE = 1,
    SC_H264_NAL_DPA = 2,
    SC_H264_NAL_DPB = 3,
    SC_H264_NAL_DPC = 4,
    SC_H264_NAL_IDR_SLICE = 5,
    SC_H264_NAL_SEI = 6,
    SC_H264_NAL_SPS = 7,       // Sequence Parameter Set
    SC_H264_NAL_PPS = 8,       // Picture Parameter Set
    SC_H264_NAL_AUD = 9,       // Access Unit Delimiter
    SC_H264_NAL_END_SEQUENCE = 10,
    SC_H264_NAL_END_STREAM = 11,
    SC_H264_NAL_FILLER_DATA = 12,
    SC_H264_NAL_SPS_EXT = 13,
    SC_H264_NAL_PREFIX = 14,
    SC_H264_NAL_SUB_SPS = 15,
    SC_H264_NAL_STAP_A = 24,   // Single-Time Aggregation Packet
    SC_H264_NAL_STAP_B = 25,
    SC_H264_NAL_MTAP16 = 26,
    SC_H264_NAL_MTAP24 = 27,
    SC_H264_NAL_FU_A = 28,     // Fragmentation Unit
    SC_H264_NAL_FU_B = 29,
};

/**
 * RTP 包回調函數
 *
 * @param rtp_data RTP 包數據
 * @param rtp_size RTP 包大小
 * @param timestamp RTP 時間戳
 * @param userdata 用戶數據
 */
typedef void (*sc_rtp_packet_callback)(const uint8_t *rtp_data,
                                       size_t rtp_size,
                                       uint32_t timestamp,
                                       void *userdata);

/**
 * RTP packetizer 結構
 */
struct sc_rtp_packetizer {
    // RTP 參數
    uint32_t ssrc;              // Synchronization Source
    uint16_t sequence_number;   // RTP 序列號
    uint32_t timestamp;         // RTP 時間戳
    uint8_t payload_type;       // RTP payload type (H.264 通常是 96-127)

    // H.264 參數
    uint8_t *sps;               // Sequence Parameter Set
    size_t sps_size;
    uint8_t *pps;               // Picture Parameter Set
    size_t pps_size;

    // MTU 設置
    size_t max_payload_size;    // 最大 RTP payload 大小（默認 1200）

    // 回調函數
    sc_rtp_packet_callback on_rtp_packet;
    void *userdata;

    // 統計信息
    uint64_t packets_sent;
    uint64_t bytes_sent;
};

/**
 * 初始化 RTP packetizer
 *
 * @param packetizer RTP packetizer 實例
 * @param ssrc SSRC (Synchronization Source)
 * @param payload_type RTP payload type (96-127)
 * @param max_payload_size 最大 payload 大小（默認 1200）
 * @param callback RTP 包回調函數
 * @param userdata 回調函數用戶數據
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_rtp_packetizer_init(struct sc_rtp_packetizer *packetizer,
                       uint32_t ssrc,
                       uint8_t payload_type,
                       size_t max_payload_size,
                       sc_rtp_packet_callback callback,
                       void *userdata);

/**
 * 設置 SPS (Sequence Parameter Set)
 *
 * @param packetizer RTP packetizer 實例
 * @param sps SPS 數據
 * @param sps_size SPS 大小
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_rtp_packetizer_set_sps(struct sc_rtp_packetizer *packetizer,
                          const uint8_t *sps,
                          size_t sps_size);

/**
 * 設置 PPS (Picture Parameter Set)
 *
 * @param packetizer RTP packetizer 實例
 * @param pps PPS 數據
 * @param pps_size PPS 大小
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_rtp_packetizer_set_pps(struct sc_rtp_packetizer *packetizer,
                          const uint8_t *pps,
                          size_t pps_size);

/**
 * 打包 H.264 AVPacket 成 RTP packets
 *
 * @param packetizer RTP packetizer 實例
 * @param packet FFmpeg AVPacket (包含 H.264 編碼數據)
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_rtp_packetizer_packetize(struct sc_rtp_packetizer *packetizer,
                            const AVPacket *packet);

/**
 * 打包單個 NAL unit
 *
 * @param packetizer RTP packetizer 實例
 * @param nal_data NAL unit 數據（包含 NAL header）
 * @param nal_size NAL unit 大小
 * @param timestamp RTP 時間戳
 * @param marker 是否為最後一個包（RTP marker bit）
 * @return 成功返回 true，失敗返回 false
 */
bool
sc_rtp_packetizer_packetize_nal(struct sc_rtp_packetizer *packetizer,
                                const uint8_t *nal_data,
                                size_t nal_size,
                                uint32_t timestamp,
                                bool marker);

/**
 * 獲取統計信息
 *
 * @param packetizer RTP packetizer 實例
 * @param packets_sent 輸出已發送包數
 * @param bytes_sent 輸出已發送字節數
 */
void
sc_rtp_packetizer_get_stats(struct sc_rtp_packetizer *packetizer,
                            uint64_t *packets_sent,
                            uint64_t *bytes_sent);

/**
 * 銷毀 RTP packetizer 並釋放資源
 *
 * @param packetizer RTP packetizer 實例
 */
void
sc_rtp_packetizer_destroy(struct sc_rtp_packetizer *packetizer);

#endif
