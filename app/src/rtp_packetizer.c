#include "rtp_packetizer.h"

#include <assert.h>
#include <stdlib.h>
#include <string.h>
#include <libavutil/intreadwrite.h>

#include "util/log.h"

// RTP 頭大小
#define RTP_HEADER_SIZE 12

// H.264 start code (0x00 0x00 0x00 0x01 or 0x00 0x00 0x01)
#define H264_START_CODE_4 0x00000001
#define H264_START_CODE_3 0x000001

// FU-A (Fragmentation Unit) 頭大小
#define FU_A_HEADER_SIZE 2

/**
 * 獲取 NAL unit 類型
 */
static inline uint8_t
get_nal_type(uint8_t nal_header) {
    return nal_header & 0x1F;
}

/**
 * 獲取 NAL unit NRI (nal_ref_idc)
 */
static inline uint8_t
get_nal_nri(uint8_t nal_header) {
    return (nal_header >> 5) & 0x03;
}

/**
 * 構建 RTP 頭
 */
static void
build_rtp_header(uint8_t *buffer, uint16_t sequence, uint32_t timestamp,
                uint32_t ssrc, uint8_t payload_type, bool marker) {
    // Version (2 bits) = 2, Padding (1 bit) = 0, Extension (1 bit) = 0,
    // CSRC count (4 bits) = 0
    buffer[0] = 0x80;

    // Marker (1 bit), Payload type (7 bits)
    buffer[1] = (marker ? 0x80 : 0x00) | (payload_type & 0x7F);

    // Sequence number (16 bits)
    AV_WB16(buffer + 2, sequence);

    // Timestamp (32 bits)
    AV_WB32(buffer + 4, timestamp);

    // SSRC (32 bits)
    AV_WB32(buffer + 8, ssrc);
}

/**
 * 發送單個 RTP 包
 */
static bool
send_rtp_packet(struct sc_rtp_packetizer *packetizer,
               const uint8_t *payload,
               size_t payload_size,
               uint32_t timestamp,
               bool marker) {
    assert(packetizer);
    assert(payload);

    uint8_t rtp_buffer[RTP_HEADER_SIZE + payload_size];

    // 構建 RTP 頭
    build_rtp_header(rtp_buffer, packetizer->sequence_number, timestamp,
                    packetizer->ssrc, packetizer->payload_type, marker);

    // 複製 payload
    memcpy(rtp_buffer + RTP_HEADER_SIZE, payload, payload_size);

    // 調用回調函數發送
    if (packetizer->on_rtp_packet) {
        packetizer->on_rtp_packet(rtp_buffer,
                                 RTP_HEADER_SIZE + payload_size,
                                 timestamp,
                                 packetizer->userdata);
    }

    // 更新統計和序列號
    packetizer->sequence_number++;
    packetizer->packets_sent++;
    packetizer->bytes_sent += RTP_HEADER_SIZE + payload_size;

    return true;
}

/**
 * 打包單個 NAL unit（不需要分片）
 */
static bool
send_single_nal(struct sc_rtp_packetizer *packetizer,
               const uint8_t *nal_data,
               size_t nal_size,
               uint32_t timestamp,
               bool marker) {
    LOGV("RTP: sending single NAL, type=%d, size=%zu",
         get_nal_type(nal_data[0]), nal_size);

    return send_rtp_packet(packetizer, nal_data, nal_size, timestamp, marker);
}

/**
 * 使用 FU-A 分片發送大的 NAL unit
 */
static bool
send_fragmented_nal(struct sc_rtp_packetizer *packetizer,
                   const uint8_t *nal_data,
                   size_t nal_size,
                   uint32_t timestamp,
                   bool last_nal_in_frame) {
    assert(nal_size > packetizer->max_payload_size);

    uint8_t nal_header = nal_data[0];
    uint8_t nal_type = get_nal_type(nal_header);
    uint8_t nal_nri = get_nal_nri(nal_header);

    LOGV("RTP: fragmenting NAL, type=%d, size=%zu", nal_type, nal_size);

    // FU indicator (F=0, NRI=from original, Type=28 for FU-A)
    uint8_t fu_indicator = (nal_nri << 5) | SC_H264_NAL_FU_A;

    // 跳過原始 NAL header，從 NAL payload 開始
    const uint8_t *nal_payload = nal_data + 1;
    size_t remaining = nal_size - 1;
    size_t offset = 0;

    bool first = true;

    while (remaining > 0) {
        size_t fragment_size = remaining;
        bool last_fragment = false;

        if (fragment_size > packetizer->max_payload_size - FU_A_HEADER_SIZE) {
            fragment_size = packetizer->max_payload_size - FU_A_HEADER_SIZE;
        } else {
            last_fragment = true;
        }

        // FU header (S, E, R, Type)
        uint8_t fu_header = nal_type;
        if (first) {
            fu_header |= 0x80;  // S (Start) bit
            first = false;
        }
        if (last_fragment) {
            fu_header |= 0x40;  // E (End) bit
        }

        // 構建 FU-A payload
        uint8_t fu_payload[FU_A_HEADER_SIZE + fragment_size];
        fu_payload[0] = fu_indicator;
        fu_payload[1] = fu_header;
        memcpy(fu_payload + FU_A_HEADER_SIZE, nal_payload + offset,
               fragment_size);

        // 發送 RTP 包（只在最後一個fragment且是frame最後一個NAL時設置marker）
        bool marker = last_fragment && last_nal_in_frame;
        if (!send_rtp_packet(packetizer, fu_payload,
                            FU_A_HEADER_SIZE + fragment_size,
                            timestamp, marker)) {
            return false;
        }

        offset += fragment_size;
        remaining -= fragment_size;
    }

    return true;
}

bool
sc_rtp_packetizer_init(struct sc_rtp_packetizer *packetizer,
                       uint32_t ssrc,
                       uint8_t payload_type,
                       size_t max_payload_size,
                       sc_rtp_packet_callback callback,
                       void *userdata) {
    assert(packetizer);
    assert(callback);

    packetizer->ssrc = ssrc;
    packetizer->sequence_number = 0;  // 可以隨機初始化
    packetizer->timestamp = 0;
    packetizer->payload_type = payload_type;
    packetizer->max_payload_size = max_payload_size > 0 ? max_payload_size : 1200;
    packetizer->on_rtp_packet = callback;
    packetizer->userdata = userdata;

    packetizer->sps = NULL;
    packetizer->sps_size = 0;
    packetizer->pps = NULL;
    packetizer->pps_size = 0;

    packetizer->packets_sent = 0;
    packetizer->bytes_sent = 0;

    LOGI("RTP packetizer initialized: ssrc=%u, payload_type=%u, max_payload=%zu",
         ssrc, payload_type, packetizer->max_payload_size);

    return true;
}

bool
sc_rtp_packetizer_set_sps(struct sc_rtp_packetizer *packetizer,
                          const uint8_t *sps,
                          size_t sps_size) {
    assert(packetizer);
    assert(sps);

    free(packetizer->sps);
    packetizer->sps = malloc(sps_size);
    if (!packetizer->sps) {
        LOG_OOM();
        return false;
    }

    memcpy(packetizer->sps, sps, sps_size);
    packetizer->sps_size = sps_size;

    LOGI("RTP: SPS set, size=%zu", sps_size);
    return true;
}

bool
sc_rtp_packetizer_set_pps(struct sc_rtp_packetizer *packetizer,
                          const uint8_t *pps,
                          size_t pps_size) {
    assert(packetizer);
    assert(pps);

    free(packetizer->pps);
    packetizer->pps = malloc(pps_size);
    if (!packetizer->pps) {
        LOG_OOM();
        return false;
    }

    memcpy(packetizer->pps, pps, pps_size);
    packetizer->pps_size = pps_size;

    LOGI("RTP: PPS set, size=%zu", pps_size);
    return true;
}

bool
sc_rtp_packetizer_packetize_nal(struct sc_rtp_packetizer *packetizer,
                                const uint8_t *nal_data,
                                size_t nal_size,
                                uint32_t timestamp,
                                bool marker) {
    assert(packetizer);
    assert(nal_data);

    if (nal_size == 0) {
        return true;
    }

    uint8_t nal_type = get_nal_type(nal_data[0]);

    // 提取 SPS/PPS
    if (nal_type == SC_H264_NAL_SPS) {
        sc_rtp_packetizer_set_sps(packetizer, nal_data, nal_size);
        // SPS 也需要發送
    } else if (nal_type == SC_H264_NAL_PPS) {
        sc_rtp_packetizer_set_pps(packetizer, nal_data, nal_size);
        // PPS 也需要發送
    } else if (nal_type == SC_H264_NAL_IDR_SLICE) {
        // 在發送 IDR frame 之前，先發送 SPS 和 PPS
        // 這確保瀏覽器在任何時候連接都能正確解碼
        if (packetizer->sps && packetizer->sps_size > 0) {
            LOGI("Sending SPS before IDR frame (size=%zu)", packetizer->sps_size);
            send_single_nal(packetizer, packetizer->sps, packetizer->sps_size,
                           timestamp, false);
        }
        if (packetizer->pps && packetizer->pps_size > 0) {
            LOGI("Sending PPS before IDR frame (size=%zu)", packetizer->pps_size);
            send_single_nal(packetizer, packetizer->pps, packetizer->pps_size,
                           timestamp, false);
        }
    }

    // 根據大小選擇打包方式
    if (nal_size <= packetizer->max_payload_size) {
        // 單個 NAL unit 模式
        return send_single_nal(packetizer, nal_data, nal_size, timestamp,
                              marker);
    } else {
        // FU-A 分片模式
        return send_fragmented_nal(packetizer, nal_data, nal_size, timestamp,
                                  marker);
    }
}

/**
 * 查找下一個 NAL unit start code
 */
static const uint8_t *
find_start_code(const uint8_t *data, size_t size, size_t *start_code_size) {
    if (size < 3) {
        return NULL;
    }

    for (size_t i = 0; i < size - 2; i++) {
        if (data[i] == 0 && data[i + 1] == 0) {
            if (data[i + 2] == 1) {
                *start_code_size = 3;
                return data + i;
            }
            if (i < size - 3 && data[i + 2] == 0 && data[i + 3] == 1) {
                *start_code_size = 4;
                return data + i;
            }
        }
    }

    return NULL;
}

bool
sc_rtp_packetizer_packetize(struct sc_rtp_packetizer *packetizer,
                            const AVPacket *packet) {
    assert(packetizer);
    assert(packet);

    // 計算 RTP 時間戳（假設 90kHz 時鐘頻率，H.264 標準）
    uint32_t rtp_timestamp = (uint32_t)(packet->pts * 90000 / 1000000);

    const uint8_t *data = packet->data;
    size_t remaining = packet->size;

    LOGV("RTP: packetizing AVPacket, size=%d, pts=%" PRId64,
         packet->size, packet->pts);

    // 解析 Annex B 格式的 H.264 數據（start code + NAL unit）
    while (remaining > 0) {
        size_t start_code_size = 0;
        const uint8_t *start = find_start_code(data, remaining,
                                              &start_code_size);

        if (!start) {
            // 沒有找到 start code，處理剩餘數據為一個 NAL unit
            if (remaining > 0) {
                bool marker = true;  // 最後一個 NAL
                if (!sc_rtp_packetizer_packetize_nal(packetizer, data,
                                                     remaining,
                                                     rtp_timestamp, marker)) {
                    return false;
                }
            }
            break;
        }

        // 如果不是在開頭找到 start code，處理之前的數據
        if (start != data) {
            size_t nal_size = start - data;
            bool marker = false;
            if (!sc_rtp_packetizer_packetize_nal(packetizer, data, nal_size,
                                                 rtp_timestamp, marker)) {
                return false;
            }
        }

        // 跳過 start code
        data = start + start_code_size;
        remaining = packet->size - (data - packet->data);
    }

    return true;
}

void
sc_rtp_packetizer_get_stats(struct sc_rtp_packetizer *packetizer,
                            uint64_t *packets_sent,
                            uint64_t *bytes_sent) {
    assert(packetizer);

    if (packets_sent) {
        *packets_sent = packetizer->packets_sent;
    }
    if (bytes_sent) {
        *bytes_sent = packetizer->bytes_sent;
    }
}

void
sc_rtp_packetizer_destroy(struct sc_rtp_packetizer *packetizer) {
    if (!packetizer) {
        return;
    }

    free(packetizer->sps);
    free(packetizer->pps);

    LOGD("RTP packetizer destroyed: sent %llu packets, %llu bytes",
         (unsigned long long)packetizer->packets_sent,
         (unsigned long long)packetizer->bytes_sent);
}
