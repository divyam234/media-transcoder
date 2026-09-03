#include "ffmpeg_bridge.h"

#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include <libavutil/avutil.h>
#include <libavutil/opt.h>
#include <libavfilter/avfilter.h>
#include <libavfilter/buffersrc.h>
#include <libavfilter/buffersink.h>
#include <libavutil/channel_layout.h>
#include <libavutil/samplefmt.h>
#include <libavutil/hwcontext.h>
#include <libavutil/pixdesc.h>

#include <math.h>
#include <stdarg.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if defined(__GLIBC__)
#include <malloc.h>
#endif

#include <stdint.h>

extern uintptr_t goAVIOOpen(uintptr_t factory_handle);
extern void goAVIOClose(uintptr_t stream_handle);
extern int goAVIORead(uintptr_t stream_handle, uint8_t *buf, int size);
extern int64_t goAVIOSeek(uintptr_t stream_handle, int64_t offset, int whence);

typedef struct TCAVIOInput {
    AVIOContext *ctx;
    uintptr_t factory_handle;
    uintptr_t stream_handle;
} TCAVIOInput;

static int tc_avio_read(void *opaque, uint8_t *buf, int size) {
    TCAVIOInput *input = (TCAVIOInput *)opaque;
    return input ? goAVIORead(input->stream_handle, buf, size) : AVERROR(EIO);
}

static int64_t tc_avio_seek(void *opaque, int64_t offset, int whence) {
    TCAVIOInput *input = (TCAVIOInput *)opaque;
    return input ? goAVIOSeek(input->stream_handle, offset, whence) : AVERROR(EIO);
}

static int tc_open_input(AVFormatContext **fmt, const char *input_path, TCAVIOInput **custom_input) {
    static const char prefix[] = "goavio:";
    if (!fmt || !input_path) return AVERROR(EINVAL);
    if (strncmp(input_path, prefix, sizeof(prefix) - 1) != 0) {
        return avformat_open_input(fmt, input_path, NULL, NULL);
    }

    char *end = NULL;
    unsigned long long raw = strtoull(input_path + sizeof(prefix) - 1, &end, 10);
    if (!end || (*end != '\0' && *end != '/') || raw == 0) return AVERROR(EINVAL);
    const char *name_hint = *end == '/' && end[1] != '\0' ? end + 1 : NULL;

    TCAVIOInput *input = (TCAVIOInput *)calloc(1, sizeof(*input));
    if (!input) return AVERROR(ENOMEM);
    input->factory_handle = (uintptr_t)raw;
    input->stream_handle = goAVIOOpen(input->factory_handle);
    if (!input->stream_handle) { free(input); return AVERROR(EIO); }
    unsigned char *buffer = (unsigned char *)av_malloc(64 * 1024);
    if (!buffer) {
        goAVIOClose(input->stream_handle);
        free(input);
        return AVERROR(ENOMEM);
    }
    input->ctx = avio_alloc_context(buffer, 64 * 1024, 0, input, tc_avio_read, NULL, tc_avio_seek);
    if (!input->ctx) {
        av_free(buffer);
        goAVIOClose(input->stream_handle);
        free(input);
        return AVERROR(ENOMEM);
    }
    input->ctx->seekable = AVIO_SEEKABLE_NORMAL;

    if (!*fmt) *fmt = avformat_alloc_context();
    if (!*fmt) {
        avio_context_free(&input->ctx);
        goAVIOClose(input->stream_handle);
        free(input);
        return AVERROR(ENOMEM);
    }
    (*fmt)->pb = input->ctx;
    (*fmt)->flags |= AVFMT_FLAG_CUSTOM_IO;
    int ret = avformat_open_input(fmt, name_hint, NULL, NULL);
    if (ret < 0) {
        if (*fmt) avformat_free_context(*fmt);
        *fmt = NULL;
        avio_context_free(&input->ctx);
        goAVIOClose(input->stream_handle);
        free(input);
        return ret;
    }
    if (custom_input) *custom_input = input;
    return 0;
}

static void tc_close_input(AVFormatContext **fmt, TCAVIOInput **custom_input) {
    if (fmt && *fmt) avformat_close_input(fmt);
    if (custom_input && *custom_input) {
        avio_context_free(&(*custom_input)->ctx);
        goAVIOClose((*custom_input)->stream_handle);
        free(*custom_input);
        *custom_input = NULL;
    }
}

static __thread char g_last_error[1024];
static pthread_once_t g_ffmpeg_init_once = PTHREAD_ONCE_INIT;

static void ffmpeg_init_once(void) {
#if defined(__GLIBC__)
    // Segment jobs are short-lived and highly concurrent. Keep glibc from
    // creating a large per-thread arena set that retains freed 4K frame
    // buffers for the lifetime of the server process.
    mallopt(M_ARENA_MAX, 4);
    mallopt(M_TRIM_THRESHOLD, 128 * 1024);
#endif
    av_log_set_level(AV_LOG_ERROR);
    avformat_network_init();
}

static void ffmpeg_init(void) { pthread_once(&g_ffmpeg_init_once, ffmpeg_init_once); }

static void tc_native_trim(void) {
#if defined(__GLIBC__)
    malloc_trim(0);
#endif
}

static void set_error(const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(g_last_error, sizeof(g_last_error), fmt, ap);
    va_end(ap);
}

static void set_av_error(const char *prefix, int err) {
    char buf[AV_ERROR_MAX_STRING_SIZE] = {0};
    av_strerror(err, buf, sizeof(buf));
    set_error("%s: %s (%d)", prefix, buf, err);
}


typedef struct TCSharedHWDevice {
    enum AVHWDeviceType type;
    char *device;
    AVBufferRef *ctx;
    unsigned users;
    struct TCSharedHWDevice *next;
} TCSharedHWDevice;

static pthread_mutex_t g_hw_device_mu = PTHREAD_MUTEX_INITIALIZER;
static TCSharedHWDevice *g_hw_devices = NULL;

static AVBufferRef *tc_hw_device_ref(enum AVHWDeviceType type, const char *device) {
    const char *key = device ? device : "";
    pthread_mutex_lock(&g_hw_device_mu);
    for (TCSharedHWDevice *entry = g_hw_devices; entry; entry = entry->next) {
        if (entry->type == type && strcmp(entry->device, key) == 0) {
            AVBufferRef *ref = av_buffer_ref(entry->ctx);
            if (ref) entry->users++;
            pthread_mutex_unlock(&g_hw_device_mu);
            if (!ref) set_error("av_buffer_ref shared hardware device failed");
            return ref;
        }
    }

    AVBufferRef *ctx = NULL;
    int ret = av_hwdevice_ctx_create(&ctx, type, key[0] ? key : NULL, NULL, 0);
    if (ret < 0) {
        pthread_mutex_unlock(&g_hw_device_mu);
        set_av_error("av_hwdevice_ctx_create", ret);
        return NULL;
    }
    TCSharedHWDevice *entry = (TCSharedHWDevice *)calloc(1, sizeof(*entry));
    if (!entry) {
        av_buffer_unref(&ctx);
        pthread_mutex_unlock(&g_hw_device_mu);
        set_error("calloc shared hardware device failed");
        return NULL;
    }
    entry->device = strdup(key);
    if (!entry->device) {
        free(entry);
        av_buffer_unref(&ctx);
        pthread_mutex_unlock(&g_hw_device_mu);
        set_error("strdup shared hardware device failed");
        return NULL;
    }
    entry->type = type;
    entry->ctx = ctx;
    entry->users = 1;
    entry->next = g_hw_devices;
    g_hw_devices = entry;
    AVBufferRef *ref = av_buffer_ref(ctx);
    if (!ref) {
        g_hw_devices = entry->next;
        free(entry->device);
        av_buffer_unref(&entry->ctx);
        free(entry);
    }
    pthread_mutex_unlock(&g_hw_device_mu);
    if (!ref) set_error("av_buffer_ref shared hardware device failed");
    return ref;
}

static void tc_hw_device_release(AVBufferRef *ctx) {
    if (!ctx) return;
    pthread_mutex_lock(&g_hw_device_mu);
    TCSharedHWDevice **link = &g_hw_devices;
    while (*link) {
        TCSharedHWDevice *entry = *link;
        if (entry->ctx && entry->ctx->data == ctx->data) {
            if (entry->users > 0) entry->users--;
            if (entry->users == 0) {
                *link = entry->next;
                av_buffer_unref(&entry->ctx);
                free(entry->device);
                free(entry);
            }
            break;
        }
        link = &entry->next;
    }
    pthread_mutex_unlock(&g_hw_device_mu);
}

const char *tc_last_error(void) { return g_last_error; }

static int tc_cancelled(const TCTranscodeOptions *opts) {
    return opts && opts->cancel_flag && *(opts->cancel_flag) != 0;
}

static int tc_interrupt_cb(void *opaque) {
    return tc_cancelled((const TCTranscodeOptions *)opaque);
}

volatile int *tc_cancel_alloc(void) {
    volatile int *flag = (volatile int *)calloc(1, sizeof(int));
    return flag;
}

void tc_cancel_set(volatile int *flag) {
    if (flag) *flag = 1;
}

void tc_cancel_free(volatile int *flag) {
    if (flag) free((void *)flag);
}


typedef struct TCDecoder {
    AVFormatContext *fmt;
    AVCodecContext *dec;
    AVCodecContext *adec;
    AVStream *stream;
    AVStream *audio_stream;
    int stream_index;
    int audio_stream_index;
    AVPacket *pkt;
    AVFrame *frame;
    AVFrame *audio_frame;
    AVBufferRef *hw_device_ctx;
    enum AVPixelFormat hw_pix_fmt;
    int hardware_decode;
    int persistent;
    TCAVIOInput *custom_input;
} TCDecoder;

static double stream_duration_seconds(AVFormatContext *fmt, AVStream *st) {
    if (fmt && fmt->duration > 0) return (double)fmt->duration / (double)AV_TIME_BASE;
    if (st && st->duration > 0) return (double)st->duration * av_q2d(st->time_base);
    return 0.0;
}

static double stream_fps(AVStream *st) {
    if (!st) return 0.0;
    if (st->avg_frame_rate.num > 0 && st->avg_frame_rate.den > 0) {
        double fps = av_q2d(st->avg_frame_rate);
        if (fps > 0.0 && fps < 240.0) return fps;
    }
    if (st->r_frame_rate.num > 0 && st->r_frame_rate.den > 0) {
        double fps = av_q2d(st->r_frame_rate);
        if (fps > 0.0 && fps < 240.0) return fps;
    }
    return 30.0;
}

int tc_probe(const char *input_path, TCInfo *info) {
    if (!input_path || !info) { set_error("tc_probe: nil input"); return AVERROR(EINVAL); }
    memset(info, 0, sizeof(*info));
    ffmpeg_init();

    AVFormatContext *fmt = NULL;
    TCAVIOInput *custom_input = NULL;
    int ret = tc_open_input(&fmt, input_path, &custom_input);
    if (ret < 0) { set_av_error("avformat_open_input", ret); return ret; }
    ret = avformat_find_stream_info(fmt, NULL);
    if (ret < 0) { set_av_error("avformat_find_stream_info", ret); tc_close_input(&fmt, &custom_input); return ret; }
    int idx = av_find_best_stream(fmt, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    if (idx < 0) { set_av_error("av_find_best_stream(video)", idx); tc_close_input(&fmt, &custom_input); return idx; }
    AVStream *st = fmt->streams[idx];
    info->duration = stream_duration_seconds(fmt, st);
    info->width = st->codecpar->width;
    info->height = st->codecpar->height;
    info->fps = stream_fps(st);
    for (unsigned int i = 0; i < fmt->nb_streams; i++) {
        if (fmt->streams[i] && fmt->streams[i]->codecpar && fmt->streams[i]->codecpar->codec_type == AVMEDIA_TYPE_AUDIO) info->audio_streams++;
    }
    info->has_audio = info->audio_streams > 0 ? 1 : 0;
    tc_close_input(&fmt, &custom_input);
    return 0;
}

static uint32_t reverse_bits32(uint32_t v) {
    v = ((v & 0x55555555U) << 1) | ((v >> 1) & 0x55555555U);
    v = ((v & 0x33333333U) << 2) | ((v >> 2) & 0x33333333U);
    v = ((v & 0x0F0F0F0FU) << 4) | ((v >> 4) & 0x0F0F0F0FU);
    v = (v << 24) | ((v & 0xFF00U) << 8) | ((v >> 8) & 0xFF00U) | (v >> 24);
    return v;
}

static int aac_object_type(const uint8_t *extra, int size) {
    if (!extra || size < 2) return 2;
    int object_type = (extra[0] >> 3) & 0x1F;
    if (object_type == 31 && size >= 2) object_type = 32 + (((extra[0] & 0x07) << 3) | (extra[1] >> 5));
    return object_type > 0 ? object_type : 2;
}

static void codec_string_from_parameters(const AVCodecParameters *par, char *out, size_t out_size) {
    if (!par || !out || out_size == 0) return;
    out[0] = '\0';
    const uint8_t *extra = par->extradata;
    int extra_size = par->extradata_size;
    switch (par->codec_id) {
        case AV_CODEC_ID_H264:
            if (extra && extra_size >= 4 && extra[0] == 1) snprintf(out, out_size, "avc1.%02X%02X%02X", extra[1], extra[2], extra[3]);
            else snprintf(out, out_size, "avc1");
            break;
        case AV_CODEC_ID_HEVC:
            if (extra && extra_size >= 13 && extra[0] == 1) {
                static const char spaces[] = {'\0', 'A', 'B', 'C'};
                int profile_space = (extra[1] >> 6) & 0x03;
                int tier = (extra[1] >> 5) & 0x01;
                int profile_idc = extra[1] & 0x1F;
                uint32_t compat = ((uint32_t)extra[2] << 24) | ((uint32_t)extra[3] << 16) | ((uint32_t)extra[4] << 8) | extra[5];
                compat = reverse_bits32(compat);
                int used;
                if (profile_space) used = snprintf(out, out_size, "hvc1.%c%d.%X.%c%d", spaces[profile_space], profile_idc, compat, tier ? 'H' : 'L', extra[12]);
                else used = snprintf(out, out_size, "hvc1.%d.%X.%c%d", profile_idc, compat, tier ? 'H' : 'L', extra[12]);
                int last_constraint = 11;
                while (last_constraint >= 6 && extra[last_constraint] == 0) last_constraint--;
                for (int i = 6; i <= last_constraint && used > 0 && (size_t)used < out_size; i++) used += snprintf(out + used, out_size - (size_t)used, ".%02X", extra[i]);
            } else snprintf(out, out_size, "hvc1");
            break;
        case AV_CODEC_ID_AV1:
            if (extra && extra_size >= 4) {
                int profile = (extra[1] >> 5) & 0x07;
                int level = extra[1] & 0x1F;
                int tier = (extra[2] >> 7) & 0x01;
                int high_bitdepth = (extra[2] >> 6) & 0x01;
                int twelve_bit = (extra[2] >> 5) & 0x01;
                int bitdepth = twelve_bit ? 12 : (high_bitdepth ? 10 : 8);
                snprintf(out, out_size, "av01.%d.%02d%c.%02d", profile, level, tier ? 'H' : 'M', bitdepth);
            } else snprintf(out, out_size, "av01");
            break;
        case AV_CODEC_ID_AAC:
            snprintf(out, out_size, "mp4a.40.%d", aac_object_type(extra, extra_size));
            break;
        default: {
            char tag[AV_FOURCC_MAX_STRING_SIZE] = {0};
            if (par->codec_tag) av_fourcc_make_string(tag, par->codec_tag);
            snprintf(out, out_size, "%s", tag[0] ? tag : avcodec_get_name(par->codec_id));
            break;
        }
    }
}

int tc_probe_codec(const char *input_path, int media_type, TCCodecInfo *info) {
    if (!input_path || !info) { set_error("tc_probe_codec: nil input"); return AVERROR(EINVAL); }
    memset(info, 0, sizeof(*info));
    ffmpeg_init();
    enum AVMediaType type = media_type == 1 ? AVMEDIA_TYPE_AUDIO : AVMEDIA_TYPE_VIDEO;
    AVFormatContext *fmt = NULL;
    TCAVIOInput *custom_input = NULL;
    int ret = tc_open_input(&fmt, input_path, &custom_input);
    if (ret < 0) { set_av_error("avformat_open_input codec", ret); return ret; }
    ret = avformat_find_stream_info(fmt, NULL);
    if (ret < 0) { set_av_error("avformat_find_stream_info codec", ret); tc_close_input(&fmt, &custom_input); return ret; }
    int idx = av_find_best_stream(fmt, type, -1, -1, NULL, 0);
    if (idx < 0) { set_av_error("av_find_best_stream codec", idx); tc_close_input(&fmt, &custom_input); return idx; }
    AVCodecParameters *par = fmt->streams[idx]->codecpar;
    info->media_type = media_type;
    info->codec_id = par->codec_id;
    info->profile = par->profile;
    info->level = par->level;
    info->sample_rate = par->sample_rate;
    info->channels = par->ch_layout.nb_channels;
    snprintf(info->codec_name, sizeof(info->codec_name), "%s", avcodec_get_name(par->codec_id));
    codec_string_from_parameters(par, info->codec_string, sizeof(info->codec_string));
    tc_close_input(&fmt, &custom_input);
    return 0;
}

static void decoder_close(TCDecoder *d) {
    if (!d) return;
    if (d->persistent) return;
    if (d->frame) av_frame_free(&d->frame);
    if (d->audio_frame) av_frame_free(&d->audio_frame);
    if (d->pkt) av_packet_free(&d->pkt);
    if (d->adec) avcodec_free_context(&d->adec);
    if (d->dec) avcodec_free_context(&d->dec);
    tc_hw_device_release(d->hw_device_ctx);
    av_buffer_unref(&d->hw_device_ctx);
    tc_close_input(&d->fmt, &d->custom_input);
    free(d);
}

static void decoder_destroy(TCDecoder *d) {
    if (!d) return;
    d->persistent = 0;
    decoder_close(d);
}

static enum AVPixelFormat hardware_get_format(AVCodecContext *ctx, const enum AVPixelFormat *pix_fmts) {
    TCDecoder *d = (TCDecoder *)ctx->opaque;
    if (!d) return AV_PIX_FMT_NONE;
    for (const enum AVPixelFormat *p = pix_fmts; *p != AV_PIX_FMT_NONE; p++) {
        if (*p != d->hw_pix_fmt) continue;
        if (!ctx->hw_frames_ctx) {
            AVBufferRef *frames = NULL;
            int ret = avcodec_get_hw_frames_parameters(ctx, d->hw_device_ctx, *p, &frames);
            if (ret < 0) {
                set_av_error("avcodec_get_hw_frames_parameters", ret);
                return AV_PIX_FMT_NONE;
            }
            ret = av_hwframe_ctx_init(frames);
            if (ret < 0) {
                set_av_error("av_hwframe_ctx_init hardware decode", ret);
                av_buffer_unref(&frames);
                return AV_PIX_FMT_NONE;
            }
            ctx->hw_frames_ctx = frames;
        }
        return *p;
    }
    set_error("decoder does not expose requested hardware pixel format %s", av_get_pix_fmt_name(d->hw_pix_fmt));
    return AV_PIX_FMT_NONE;
}



static int decoder_supports_hw(const AVCodec *codec, enum AVHWDeviceType device_type, enum AVPixelFormat pix_fmt) {
    for (int i = 0;; i++) {
        const AVCodecHWConfig *cfg = avcodec_get_hw_config(codec, i);
        if (!cfg) return 0;
        if ((cfg->methods & AV_CODEC_HW_CONFIG_METHOD_HW_DEVICE_CTX) && cfg->device_type == device_type && cfg->pix_fmt == pix_fmt) return 1;
    }
}

static enum AVHWDeviceType hardware_device_type_for_encoder(const char *encoder_name) {
    if (encoder_name && strstr(encoder_name, "_nvenc")) return AV_HWDEVICE_TYPE_CUDA;
    if (encoder_name && strstr(encoder_name, "_vaapi")) return AV_HWDEVICE_TYPE_VAAPI;
    return AV_HWDEVICE_TYPE_NONE;
}

static enum AVPixelFormat hardware_pix_fmt_for_device(enum AVHWDeviceType device_type) {
    if (device_type == AV_HWDEVICE_TYPE_CUDA) return AV_PIX_FMT_CUDA;
    if (device_type == AV_HWDEVICE_TYPE_VAAPI) return AV_PIX_FMT_VAAPI;
    return AV_PIX_FMT_NONE;
}

static TCDecoder *decoder_open(const char *input_path, const char *encoder_name, const char *hardware_device, int hardware_decode) {
    if (!input_path) { set_error("decoder_open: nil input"); return NULL; }
    ffmpeg_init();
    TCDecoder *d = (TCDecoder *)calloc(1, sizeof(TCDecoder));
    if (!d) { set_error("calloc decoder failed"); return NULL; }

    d->fmt = avformat_alloc_context();
    if (!d->fmt) { set_error("avformat_alloc_context failed"); decoder_close(d); return NULL; }
    d->fmt->flags |= AVFMT_FLAG_GENPTS;

    int ret = tc_open_input(&d->fmt, input_path, &d->custom_input);
    if (ret < 0) { set_av_error("avformat_open_input", ret); decoder_close(d); return NULL; }
    ret = avformat_find_stream_info(d->fmt, NULL);
    if (ret < 0) { set_av_error("avformat_find_stream_info", ret); decoder_close(d); return NULL; }
    d->stream_index = av_find_best_stream(d->fmt, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    if (d->stream_index < 0) { set_av_error("av_find_best_stream(video)", d->stream_index); decoder_close(d); return NULL; }
    d->stream = d->fmt->streams[d->stream_index];
    d->audio_stream_index = av_find_best_stream(d->fmt, AVMEDIA_TYPE_AUDIO, -1, d->stream_index, NULL, 0);
    d->audio_stream = d->audio_stream_index >= 0 ? d->fmt->streams[d->audio_stream_index] : NULL;

    const AVCodec *codec = avcodec_find_decoder(d->stream->codecpar->codec_id);
    if (!codec) { set_error("decoder not found for codec id %d", d->stream->codecpar->codec_id); decoder_close(d); return NULL; }
    d->dec = avcodec_alloc_context3(codec);
    if (!d->dec) { set_error("avcodec_alloc_context3 failed"); decoder_close(d); return NULL; }
    ret = avcodec_parameters_to_context(d->dec, d->stream->codecpar);
    if (ret < 0) { set_av_error("avcodec_parameters_to_context", ret); decoder_close(d); return NULL; }
    if (hardware_decode) {
        enum AVHWDeviceType device_type = hardware_device_type_for_encoder(encoder_name);
        enum AVPixelFormat hw_pix_fmt = hardware_pix_fmt_for_device(device_type);
        if (device_type == AV_HWDEVICE_TYPE_NONE || hw_pix_fmt == AV_PIX_FMT_NONE) {
            set_error("hardware decode is supported for VAAPI and NVENC encoders");
            decoder_close(d);
            return NULL;
        }
        if (!decoder_supports_hw(codec, device_type, hw_pix_fmt)) {
            set_error("decoder %s does not support %s hardware decode", codec->name, av_hwdevice_get_type_name(device_type));
            decoder_close(d);
            return NULL;
        }
        const char *device = hardware_device && hardware_device[0] ? hardware_device : (device_type == AV_HWDEVICE_TYPE_CUDA ? "0" : "/dev/dri/renderD128");
        d->hw_device_ctx = tc_hw_device_ref(device_type, device);
        if (!d->hw_device_ctx) { decoder_close(d); return NULL; }
        d->dec->opaque = d;
        d->dec->get_format = hardware_get_format;
        d->dec->hw_device_ctx = av_buffer_ref(d->hw_device_ctx);
        d->hw_pix_fmt = hw_pix_fmt;
        d->hardware_decode = 1;
    }
    ret = avcodec_open2(d->dec, codec, NULL);
    if (ret < 0) { set_av_error("avcodec_open2 decoder", ret); decoder_close(d); return NULL; }
    if (d->audio_stream) {
        const AVCodec *acodec = avcodec_find_decoder(d->audio_stream->codecpar->codec_id);
        if (acodec) {
            d->adec = avcodec_alloc_context3(acodec);
            if (!d->adec) { set_error("avcodec_alloc_context3 audio decoder failed"); decoder_close(d); return NULL; }
            ret = avcodec_parameters_to_context(d->adec, d->audio_stream->codecpar);
            if (ret < 0) { set_av_error("avcodec_parameters_to_context audio", ret); decoder_close(d); return NULL; }
            ret = avcodec_open2(d->adec, acodec, NULL);
            if (ret < 0) { set_av_error("avcodec_open2 audio decoder", ret); decoder_close(d); return NULL; }
        }
    }
    d->pkt = av_packet_alloc();
    d->frame = av_frame_alloc();
    d->audio_frame = av_frame_alloc();
    if (!d->pkt || !d->frame || !d->audio_frame) { set_error("packet/frame alloc failed"); decoder_close(d); return NULL; }
    return d;
}


static int decoder_prepare_hw_frames(TCDecoder *d, int64_t seek_ts) {
    if (!d || !d->hardware_decode || d->dec->hw_frames_ctx) return 0;

    int ret = 0;
    int got_frame = 0;
    while ((ret = av_read_frame(d->fmt, d->pkt)) >= 0) {
        if (d->pkt->stream_index != d->stream_index) {
            av_packet_unref(d->pkt);
            continue;
        }
        ret = avcodec_send_packet(d->dec, d->pkt);
        av_packet_unref(d->pkt);
        if (ret < 0 && ret != AVERROR(EAGAIN)) {
            set_av_error("hardware prime avcodec_send_packet", ret);
            return ret;
        }
        while ((ret = avcodec_receive_frame(d->dec, d->frame)) >= 0) {
            av_frame_unref(d->frame);
            if (!d->dec->hw_frames_ctx) {
                set_error("hardware decoder did not initialize hardware frames context");
                return AVERROR(EINVAL);
            }
            got_frame = 1;
            break;
        }
        if (got_frame) break;
        if (ret != AVERROR(EAGAIN) && ret != AVERROR_EOF) {
            set_av_error("hardware prime avcodec_receive_frame", ret);
            return ret;
        }
    }
    if (!got_frame) {
        if (ret < 0 && ret != AVERROR_EOF) set_av_error("hardware prime av_read_frame", ret);
        else set_error("hardware decoder did not produce a frame");
        return ret < 0 && ret != AVERROR_EOF ? ret : AVERROR(EINVAL);
    }

    ret = avformat_seek_file(d->fmt, d->stream_index, INT64_MIN, seek_ts, seek_ts, AVSEEK_FLAG_BACKWARD);
    if (ret < 0) {
        set_av_error("hardware prime avformat_seek_file", ret);
        return ret;
    }
    avcodec_flush_buffers(d->dec);
    return 0;
}

static const AVCodec *choose_video_encoder(const char *encoder_name, enum AVCodecID *codec_id) {
    const AVCodec *codec = NULL;
    if (encoder_name && encoder_name[0]) {
        codec = avcodec_find_encoder_by_name(encoder_name);
        if (codec) { *codec_id = codec->id; return codec; }
        set_error("requested encoder %s is not available in this FFmpeg build", encoder_name);
        *codec_id = AV_CODEC_ID_NONE;
        return NULL;
    }
    codec = avcodec_find_encoder_by_name("libx264");
    if (codec) { *codec_id = codec->id; return codec; }
    codec = avcodec_find_encoder(AV_CODEC_ID_H264);
    if (codec) { *codec_id = AV_CODEC_ID_H264; return codec; }
    codec = avcodec_find_encoder(AV_CODEC_ID_MPEG4);
    if (codec) { *codec_id = AV_CODEC_ID_MPEG4; return codec; }
    *codec_id = AV_CODEC_ID_NONE;
    return NULL;
}

static int encode_video_frame(AVFormatContext *ofmt, AVCodecContext *enc, AVStream *out_st, AVPacket *pkt, AVFrame *frame) {
    int ret = avcodec_send_frame(enc, frame);
    if (ret < 0) { set_av_error("avcodec_send_frame", ret); return ret; }
    while ((ret = avcodec_receive_packet(enc, pkt)) >= 0) {
        if (pkt->duration <= 0) pkt->duration = 1;
        av_packet_rescale_ts(pkt, enc->time_base, out_st->time_base);
        pkt->stream_index = out_st->index;
        pkt->pos = -1;
        ret = av_interleaved_write_frame(ofmt, pkt);
        av_packet_unref(pkt);
        if (ret < 0) { set_av_error("encode av_interleaved_write_frame", ret); return ret; }
    }
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) return 0;
    set_av_error("avcodec_receive_packet", ret);
    return ret;
}

static int drain_filter_to_encoder(
    AVFilterContext *sink_ctx,
    AVRational sink_tb,
    AVFormatContext *ofmt,
    AVCodecContext *enc,
    AVBufferRef *hw_frames_ctx,
    AVStream *out_st,
    AVPacket *enc_pkt,
    AVFrame *filt_frame,
    int64_t *first_pts,
    int64_t *last_pts,
    int64_t *fallback_pts,
    int64_t pts_offset
) {
    int ret = 0;
    while ((ret = av_buffersink_get_frame(sink_ctx, filt_frame)) >= 0) {
        int64_t out_pts;
        if (filt_frame->pts != AV_NOPTS_VALUE) {
            if (*first_pts == AV_NOPTS_VALUE) *first_pts = filt_frame->pts;
            out_pts = av_rescale_q(filt_frame->pts - *first_pts, sink_tb, enc->time_base);
        } else {
            out_pts = (*fallback_pts)++;
        }
        out_pts += pts_offset;
        if (*last_pts != AV_NOPTS_VALUE && out_pts <= *last_pts) out_pts = *last_pts + 1;
        filt_frame->pts = out_pts;
        filt_frame->duration = 1;
        filt_frame->pict_type = AV_PICTURE_TYPE_NONE;
        filt_frame->flags &= ~AV_FRAME_FLAG_KEY;
        *last_pts = out_pts;
        if (filt_frame->format == AV_PIX_FMT_VAAPI || filt_frame->format == AV_PIX_FMT_CUDA) {
            ret = encode_video_frame(ofmt, enc, out_st, enc_pkt, filt_frame);
        } else if (hw_frames_ctx) {
            AVFrame *hw_frame = av_frame_alloc();
            if (!hw_frame) { av_frame_unref(filt_frame); return AVERROR(ENOMEM); }
            hw_frame->format = AV_PIX_FMT_VAAPI;
            hw_frame->width = enc->width;
            hw_frame->height = enc->height;
            ret = av_hwframe_get_buffer(hw_frames_ctx, hw_frame, 0);
            if (ret < 0) { set_av_error("av_hwframe_get_buffer", ret); av_frame_free(&hw_frame); av_frame_unref(filt_frame); return ret; }
            ret = av_hwframe_transfer_data(hw_frame, filt_frame, 0);
            if (ret < 0) { set_av_error("av_hwframe_transfer_data", ret); av_frame_free(&hw_frame); av_frame_unref(filt_frame); return ret; }
            ret = av_frame_copy_props(hw_frame, filt_frame);
            if (ret < 0) { set_av_error("av_frame_copy_props", ret); av_frame_free(&hw_frame); av_frame_unref(filt_frame); return ret; }
            hw_frame->pts = filt_frame->pts;
            hw_frame->duration = filt_frame->duration;
            ret = encode_video_frame(ofmt, enc, out_st, enc_pkt, hw_frame);
            av_frame_free(&hw_frame);
        } else {
            ret = encode_video_frame(ofmt, enc, out_st, enc_pkt, filt_frame);
        }
        av_frame_unref(filt_frame);
        if (ret < 0) return ret;
    }
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) return 0;
    set_av_error("filter av_buffersink_get_frame", ret);
    return ret;
}

static int init_video_filter(
    TCDecoder *dec,
    int target_width,
    int crop_width,
    int crop_height,
    int crop_x,
    int crop_y,
    AVRational fps,
    enum AVPixelFormat output_pix_fmt,
    int use_vaapi,
    int use_cuda,
    AVBufferRef *hw_device_ctx,
    AVBufferRef *hw_frames_ctx,
    AVFilterGraph **graph_out,
    AVFilterContext **src_out,
    AVFilterContext **sink_out,
    int *out_w,
    int *out_h,
    AVRational *sink_tb
) {
    int ret = 0;
    char args[512];
    char filter_descr[256];
    const AVFilter *buffersrc = avfilter_get_by_name("buffer");
    const AVFilter *buffersink = avfilter_get_by_name("buffersink");
    AVFilterInOut *outputs = avfilter_inout_alloc();
    AVFilterInOut *inputs = avfilter_inout_alloc();
    AVFilterGraph *graph = avfilter_graph_alloc();
    AVFilterContext *src_ctx = NULL;
    AVFilterContext *sink_ctx = NULL;
    if (!outputs || !inputs || !graph) { set_error("filter graph allocation failed"); ret = AVERROR(ENOMEM); goto fail; }
    if (!buffersrc || !buffersink) { set_error("required buffer filters not available"); ret = AVERROR_FILTER_NOT_FOUND; goto fail; }

    AVRational sar = dec->stream->sample_aspect_ratio.num > 0 ? dec->stream->sample_aspect_ratio : dec->dec->sample_aspect_ratio;
    if (sar.num <= 0 || sar.den <= 0) sar = (AVRational){1, 1};
    AVRational tb = dec->stream->time_base;
    if (tb.num <= 0 || tb.den <= 0) tb = (AVRational){1, 1000000};

    enum AVPixelFormat source_pix_fmt = use_cuda ? AV_PIX_FMT_CUDA : (use_vaapi ? AV_PIX_FMT_VAAPI : dec->dec->pix_fmt);
    snprintf(args, sizeof(args), "video_size=%dx%d:pix_fmt=%d:time_base=%d/%d:pixel_aspect=%d/%d",
        dec->dec->width, dec->dec->height, source_pix_fmt, tb.num, tb.den, sar.num, sar.den);
    if (use_vaapi || use_cuda) {
        src_ctx = avfilter_graph_alloc_filter(graph, buffersrc, "in");
        if (!src_ctx) { ret = AVERROR(ENOMEM); goto fail; }
        AVBufferSrcParameters *params = av_buffersrc_parameters_alloc();
        if (!params) { ret = AVERROR(ENOMEM); goto fail; }
        params->format = source_pix_fmt;
        params->width = dec->dec->width;
        params->height = dec->dec->height;
        params->time_base = tb;
        params->sample_aspect_ratio = sar;
        params->hw_frames_ctx = av_buffer_ref(hw_frames_ctx);
        ret = av_buffersrc_parameters_set(src_ctx, params);
        av_buffer_unref(&params->hw_frames_ctx);
        av_free(params);
        if (ret < 0) { set_av_error("av_buffersrc_parameters_set hardware", ret); goto fail; }
        ret = avfilter_init_str(src_ctx, NULL);
        if (ret < 0) { set_av_error("avfilter_init_str hardware buffer", ret); goto fail; }
    } else {
        ret = avfilter_graph_create_filter(&src_ctx, buffersrc, "in", args, NULL, graph);
        if (ret < 0) { set_av_error("avfilter_graph_create_filter buffer", ret); goto fail; }
    }
    ret = avfilter_graph_create_filter(&sink_ctx, buffersink, "out", NULL, NULL, graph);
    if (ret < 0) { set_av_error("avfilter_graph_create_filter buffersink", ret); goto fail; }

    const char *pix_fmt_name = av_get_pix_fmt_name(output_pix_fmt);
    if (!pix_fmt_name) { set_error("unsupported filter output pixel format"); ret = AVERROR(EINVAL); goto fail; }
    int use_crop = crop_width > 0 && crop_height > 0;
    if (use_vaapi) {
        if (use_crop && target_width > 0) {
            int target_height = ((int)llround((double)target_width * (double)crop_height / (double)crop_width)) & ~1;
            if (target_height < 2) target_height = 2;
            snprintf(filter_descr, sizeof(filter_descr), "crop=%d:%d:%d:%d,fps=%d/%d,scale_vaapi=w=%d:h=%d:format=%s", crop_width, crop_height, crop_x, crop_y, fps.num, fps.den, target_width, target_height, pix_fmt_name);
        } else if (use_crop) {
            snprintf(filter_descr, sizeof(filter_descr), "crop=%d:%d:%d:%d,fps=%d/%d,scale_vaapi=w=%d:h=%d:format=%s", crop_width, crop_height, crop_x, crop_y, fps.num, fps.den, crop_width, crop_height, pix_fmt_name);
        } else if (target_width > 0) snprintf(filter_descr, sizeof(filter_descr), "fps=%d/%d,scale_vaapi=w=%d:h=-2:format=%s", fps.num, fps.den, target_width, pix_fmt_name);
        else snprintf(filter_descr, sizeof(filter_descr), "fps=%d/%d,scale_vaapi=format=%s", fps.num, fps.den, pix_fmt_name);
    } else if (use_cuda) {
        if (use_crop && target_width > 0) {
            int target_height = ((int)llround((double)target_width * (double)crop_height / (double)crop_width)) & ~1;
            if (target_height < 2) target_height = 2;
            snprintf(filter_descr, sizeof(filter_descr), "crop=%d:%d:%d:%d,fps=%d/%d,scale_cuda=w=%d:h=%d:format=%s", crop_width, crop_height, crop_x, crop_y, fps.num, fps.den, target_width, target_height, pix_fmt_name);
        } else if (use_crop) {
            snprintf(filter_descr, sizeof(filter_descr), "crop=%d:%d:%d:%d,fps=%d/%d,scale_cuda=w=%d:h=%d:format=%s", crop_width, crop_height, crop_x, crop_y, fps.num, fps.den, crop_width, crop_height, pix_fmt_name);
        } else if (target_width > 0) snprintf(filter_descr, sizeof(filter_descr), "fps=%d/%d,scale_cuda=w=%d:h=-2:format=%s", fps.num, fps.den, target_width, pix_fmt_name);
        else snprintf(filter_descr, sizeof(filter_descr), "fps=%d/%d,scale_cuda=format=%s", fps.num, fps.den, pix_fmt_name);
    } else {
        if (use_crop && target_width > 0) snprintf(filter_descr, sizeof(filter_descr), "crop=%d:%d:%d:%d,scale=%d:-2:flags=fast_bilinear,fps=%d/%d,format=%s", crop_width, crop_height, crop_x, crop_y, target_width, fps.num, fps.den, pix_fmt_name);
        else if (use_crop) snprintf(filter_descr, sizeof(filter_descr), "crop=%d:%d:%d:%d,fps=%d/%d,format=%s", crop_width, crop_height, crop_x, crop_y, fps.num, fps.den, pix_fmt_name);
        else if (target_width > 0) snprintf(filter_descr, sizeof(filter_descr), "scale=%d:-2:flags=fast_bilinear,fps=%d/%d,format=%s", target_width, fps.num, fps.den, pix_fmt_name);
        else snprintf(filter_descr, sizeof(filter_descr), "fps=%d/%d,format=%s", fps.num, fps.den, pix_fmt_name);
    }

    outputs->name = av_strdup("in"); outputs->filter_ctx = src_ctx; outputs->pad_idx = 0; outputs->next = NULL;
    inputs->name = av_strdup("out"); inputs->filter_ctx = sink_ctx; inputs->pad_idx = 0; inputs->next = NULL;
    if (!outputs->name || !inputs->name) { set_error("av_strdup filter endpoint failed"); ret = AVERROR(ENOMEM); goto fail; }
    ret = avfilter_graph_parse_ptr(graph, filter_descr, &inputs, &outputs, NULL);
    if (ret < 0) { set_av_error("avfilter_graph_parse_ptr", ret); goto fail; }
    if (use_vaapi || use_cuda) {
        const char *scale_filter = use_cuda ? "scale_cuda" : "scale_vaapi";
        for (unsigned int i = 0; i < graph->nb_filters; i++) {
            AVFilterContext *f = graph->filters[i];
            if (f && f->filter && strcmp(f->filter->name, scale_filter) == 0) f->hw_device_ctx = av_buffer_ref(hw_device_ctx);
        }
    }
    ret = avfilter_graph_config(graph, NULL);
    if (ret < 0) { set_av_error("avfilter_graph_config", ret); goto fail; }

    *graph_out = graph; *src_out = src_ctx; *sink_out = sink_ctx;
    *out_w = av_buffersink_get_w(sink_ctx); *out_h = av_buffersink_get_h(sink_ctx);
    *sink_tb = av_buffersink_get_time_base(sink_ctx);
    avfilter_inout_free(&inputs); avfilter_inout_free(&outputs);
    return 0;
fail:
    avfilter_inout_free(&inputs); avfilter_inout_free(&outputs);
    if (graph) avfilter_graph_free(&graph);
    return ret < 0 ? ret : AVERROR(EINVAL);
}

static int opt_int(int value, int fallback) { return value > 0 ? value : fallback; }
static const char *opt_str(const char *value, const char *fallback) { return value && value[0] ? value : fallback; }

static void set_mux_options(AVDictionary **mux_opts, const TCTranscodeOptions *opts) {
    if (!opts) { av_dict_set(mux_opts, "movflags", "+faststart", 0); return; }
    const char *format = opts->format && opts->format[0] ? opts->format : "";
    if (strcmp(format, "dash") == 0) {
        av_dict_set(mux_opts, "seg_duration", "4", 0);
        av_dict_set(mux_opts, "use_template", "1", 0);
        av_dict_set(mux_opts, "use_timeline", "1", 0);
        return;
    }
    if (strcmp(format, "hls") == 0) {
        char buf[128];
        double hls_time = opts->hls_time > 0.0 ? opts->hls_time : 4.0;
        snprintf(buf, sizeof(buf), "%.6f", hls_time); av_dict_set(mux_opts, "hls_time", buf, 0);
        snprintf(buf, sizeof(buf), "%d", opts->hls_list_size >= 0 ? opts->hls_list_size : 0); av_dict_set(mux_opts, "hls_list_size", buf, 0);
        av_dict_set(mux_opts, "hls_playlist_type", opt_str(opts->hls_playlist_type, "vod"), 0);
        av_dict_set(mux_opts, "hls_segment_type", opt_str(opts->hls_segment_type, "mpegts"), 0);
        av_dict_set(mux_opts, "start_number", "0", 0);
        if (opts->segment_filename && opts->segment_filename[0]) av_dict_set(mux_opts, "hls_segment_filename", opts->segment_filename, 0);
        return;
    }
    if (strcmp(format, "mp4") == 0 && opts->duration > 0.0 && !opts->faststart) {
        av_dict_set(mux_opts, "movflags", "frag_keyframe+empty_moov+default_base_moof+omit_tfhd_offset", 0);
        return;
    }
    if (opts->faststart) av_dict_set(mux_opts, "movflags", "+faststart", 0);
}


static const AVCodec *choose_audio_encoder(const char *name, enum AVCodecID *codec_id) {
    const AVCodec *codec = NULL;
    if (name && name[0]) {
        codec = avcodec_find_encoder_by_name(name);
        if (codec) { *codec_id = codec->id; return codec; }
        set_error("requested audio encoder %s is not available", name);
        *codec_id = AV_CODEC_ID_NONE;
        return NULL;
    }
    codec = avcodec_find_encoder_by_name("aac");
    if (codec) { *codec_id = codec->id; return codec; }
    codec = avcodec_find_encoder(AV_CODEC_ID_AAC);
    if (codec) { *codec_id = AV_CODEC_ID_AAC; return codec; }
    *codec_id = AV_CODEC_ID_NONE;
    return NULL;
}

static int encode_audio_frame(AVFormatContext *ofmt, AVCodecContext *aenc, AVStream *out_st, AVPacket *pkt, AVFrame *frame) {
    int ret = avcodec_send_frame(aenc, frame);
    if (ret < 0) { set_av_error("avcodec_send_frame audio", ret); return ret; }
    while ((ret = avcodec_receive_packet(aenc, pkt)) >= 0) {
        if (pkt->pts != AV_NOPTS_VALUE && pkt->pts < 0) pkt->pts = 0;
        if (pkt->dts != AV_NOPTS_VALUE && pkt->dts < 0) pkt->dts = 0;
        av_packet_rescale_ts(pkt, aenc->time_base, out_st->time_base);
        pkt->stream_index = out_st->index;
        pkt->pos = -1;
        ret = av_interleaved_write_frame(ofmt, pkt);
        av_packet_unref(pkt);
        if (ret < 0) { set_av_error("audio encode write", ret); return ret; }
    }
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) return 0;
    set_av_error("avcodec_receive_packet audio", ret);
    return ret;
}

static int init_audio_filter(
    TCDecoder *dec,
    AVCodecContext *aenc,
    double start_time,
    double duration_limit,
    AVFilterGraph **graph_out,
    AVFilterContext **src_out,
    AVFilterContext **sink_out,
    AVRational *sink_tb
) {
    if (!dec || !dec->adec || !dec->audio_stream || !aenc) return AVERROR(EINVAL);
    int ret = 0;
    char args[1024];
    char ch_layout[128];
    char filter_descr[512];
    const AVFilter *buffersrc = avfilter_get_by_name("abuffer");
    const AVFilter *buffersink = avfilter_get_by_name("abuffersink");
    AVFilterInOut *outputs = avfilter_inout_alloc();
    AVFilterInOut *inputs = avfilter_inout_alloc();
    AVFilterGraph *graph = avfilter_graph_alloc();
    AVFilterContext *src_ctx = NULL;
    AVFilterContext *sink_ctx = NULL;
    if (!outputs || !inputs || !graph) { set_error("audio filter graph allocation failed"); ret = AVERROR(ENOMEM); goto fail; }
    if (!buffersrc || !buffersink) { set_error("required audio buffer filters not available"); ret = AVERROR_FILTER_NOT_FOUND; goto fail; }

    AVChannelLayout in_layout = dec->adec->ch_layout;
    if (in_layout.nb_channels <= 0) av_channel_layout_default(&in_layout, dec->adec->ch_layout.nb_channels > 0 ? dec->adec->ch_layout.nb_channels : 2);
    ret = av_channel_layout_describe(&in_layout, ch_layout, sizeof(ch_layout));
    if (ret < 0) { set_av_error("av_channel_layout_describe", ret); goto fail; }
    const char *fmt_name = av_get_sample_fmt_name(dec->adec->sample_fmt);
    if (!fmt_name) fmt_name = "fltp";
    AVRational tb = dec->audio_stream->time_base;
    if (tb.num <= 0 || tb.den <= 0) tb = (AVRational){1, dec->adec->sample_rate > 0 ? dec->adec->sample_rate : 48000};
    int sample_rate = dec->adec->sample_rate > 0 ? dec->adec->sample_rate : 48000;
    snprintf(args, sizeof(args), "time_base=%d/%d:sample_rate=%d:sample_fmt=%s:channel_layout=%s", tb.num, tb.den, sample_rate, fmt_name, ch_layout);
    ret = avfilter_graph_create_filter(&src_ctx, buffersrc, "in", args, NULL, graph);
    if (ret < 0) { set_av_error("avfilter_graph_create_filter abuffer", ret); goto fail; }
    ret = avfilter_graph_create_filter(&sink_ctx, buffersink, "out", NULL, NULL, graph);
    if (ret < 0) { set_av_error("avfilter_graph_create_filter abuffersink", ret); goto fail; }

    double end_time = duration_limit > 0.0 ? start_time + duration_limit : 0.0;
    if (duration_limit > 0.0) {
        snprintf(filter_descr, sizeof(filter_descr), "atrim=start=%.9f:end=%.9f,asetpts=PTS-STARTPTS,aformat=sample_fmts=fltp:sample_rates=%d:channel_layouts=stereo,asetnsamples=n=%d:p=0", start_time, end_time, aenc->sample_rate, aenc->frame_size > 0 ? aenc->frame_size : 1024);
    } else if (start_time > 0.0) {
        snprintf(filter_descr, sizeof(filter_descr), "atrim=start=%.9f,asetpts=PTS-STARTPTS,aformat=sample_fmts=fltp:sample_rates=%d:channel_layouts=stereo,asetnsamples=n=%d:p=0", start_time, aenc->sample_rate, aenc->frame_size > 0 ? aenc->frame_size : 1024);
    } else {
        snprintf(filter_descr, sizeof(filter_descr), "asetpts=PTS-STARTPTS,aformat=sample_fmts=fltp:sample_rates=%d:channel_layouts=stereo,asetnsamples=n=%d:p=0", aenc->sample_rate, aenc->frame_size > 0 ? aenc->frame_size : 1024);
    }

    outputs->name = av_strdup("in"); outputs->filter_ctx = src_ctx; outputs->pad_idx = 0; outputs->next = NULL;
    inputs->name = av_strdup("out"); inputs->filter_ctx = sink_ctx; inputs->pad_idx = 0; inputs->next = NULL;
    if (!outputs->name || !inputs->name) { set_error("av_strdup audio filter endpoint failed"); ret = AVERROR(ENOMEM); goto fail; }
    ret = avfilter_graph_parse_ptr(graph, filter_descr, &inputs, &outputs, NULL);
    if (ret < 0) { set_av_error("avfilter_graph_parse_ptr audio", ret); goto fail; }
    ret = avfilter_graph_config(graph, NULL);
    if (ret < 0) { set_av_error("avfilter_graph_config audio", ret); goto fail; }
    *graph_out = graph; *src_out = src_ctx; *sink_out = sink_ctx; *sink_tb = av_buffersink_get_time_base(sink_ctx);
    avfilter_inout_free(&inputs); avfilter_inout_free(&outputs);
    return 0;
fail:
    avfilter_inout_free(&inputs); avfilter_inout_free(&outputs);
    if (graph) avfilter_graph_free(&graph);
    return ret < 0 ? ret : AVERROR(EINVAL);
}

static int drain_audio_filter_to_encoder(
    AVFilterContext *sink_ctx,
    AVRational sink_tb,
    AVFormatContext *ofmt,
    AVCodecContext *aenc,
    AVStream *out_st,
    AVPacket *pkt,
    AVFrame *filt_frame,
    int64_t *last_pts,
    int64_t pts_offset
) {
    int ret = 0;
    while ((ret = av_buffersink_get_frame(sink_ctx, filt_frame)) >= 0) {
        if (filt_frame->pts != AV_NOPTS_VALUE) {
            int64_t pts = av_rescale_q(filt_frame->pts, sink_tb, aenc->time_base) + pts_offset;
            if (*last_pts != AV_NOPTS_VALUE && pts <= *last_pts) pts = *last_pts + filt_frame->nb_samples;
            filt_frame->pts = pts;
        } else {
            filt_frame->pts = (*last_pts == AV_NOPTS_VALUE) ? pts_offset : *last_pts + filt_frame->nb_samples;
        }
        *last_pts = filt_frame->pts;
        ret = encode_audio_frame(ofmt, aenc, out_st, pkt, filt_frame);
        av_frame_unref(filt_frame);
        if (ret < 0) return ret;
    }
    if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) return 0;
    set_av_error("audio filter av_buffersink_get_frame", ret);
    return ret;
}

static int transcode_decoder_to_video_opts(TCDecoder *dec, const char *output_path, const TCTranscodeOptions *opts) {
    if (!dec || !output_path) { set_error("transcode: nil input"); if (dec) decoder_close(dec); return AVERROR(EINVAL); }
    if (tc_cancelled(opts)) { set_error("transcode cancelled"); decoder_close(dec); return AVERROR_EXIT; }
    dec->fmt->interrupt_callback.callback = tc_interrupt_cb;
    dec->fmt->interrupt_callback.opaque = (void *)opts;
    int target_width = opts ? opts->target_width : 0;
    double target_fps = opts ? opts->target_fps : 0.0;
    int crf = opts ? opt_int(opts->crf, 28) : 28;
    int gop_size = opts ? opt_int(opts->gop_size, 48) : 48;
    int max_b_frames = opts ? opts->max_b_frames : 0;
    if (max_b_frames < 0) max_b_frames = 0;
    const char *preset = opts ? opt_str(opts->preset, "ultrafast") : "ultrafast";
    const char *encoder_name = (opts && opts->encoder_name && opts->encoder_name[0]) ? opts->encoder_name : NULL;
    const char *format_name = (opts && opts->format && opts->format[0]) ? opts->format : NULL;
    const char *hardware_device = (opts && opts->hardware_device && opts->hardware_device[0]) ? opts->hardware_device : NULL;
    int use_vaapi = encoder_name && strstr(encoder_name, "_vaapi") != NULL;
    int use_nvenc = encoder_name && strstr(encoder_name, "_nvenc") != NULL;
    int zero_copy_vaapi = use_vaapi && dec->hardware_decode && dec->hw_pix_fmt == AV_PIX_FMT_VAAPI;
    int zero_copy_cuda = use_nvenc && dec->hardware_decode && dec->hw_pix_fmt == AV_PIX_FMT_CUDA;
    double start_time = (opts && opts->start_time > 0.0) ? opts->start_time : 0.0;
    double duration_limit = (opts && opts->duration > 0.0) ? opts->duration : 0.0;
    double end_time = duration_limit > 0.0 ? start_time + duration_limit : 0.0;
    double timestamp_offset = (opts && opts->timestamp_offset > 0.0) ? opts->timestamp_offset : 0.0;

    int64_t seek_ts = 0;
    if (start_time > 0.0 || dec->persistent) {
        seek_ts = av_rescale_q((int64_t)llround(start_time * (double)AV_TIME_BASE), AV_TIME_BASE_Q, dec->stream->time_base);
        int ret_seek = avformat_seek_file(dec->fmt, dec->stream_index, INT64_MIN, seek_ts, seek_ts, AVSEEK_FLAG_BACKWARD);
        if (ret_seek < 0) { set_av_error("avformat_seek_file", ret_seek); decoder_close(dec); return ret_seek; }
        avcodec_flush_buffers(dec->dec);
        if (dec->adec) avcodec_flush_buffers(dec->adec);
    }
    if (zero_copy_vaapi || zero_copy_cuda) {
        int ret_prepare = decoder_prepare_hw_frames(dec, seek_ts);
        if (ret_prepare < 0) { decoder_close(dec); return ret_prepare; }
    }

    double fps_d = target_fps > 1.0 && target_fps <= 120.0 ? target_fps : stream_fps(dec->stream);
    if (fps_d <= 1.0 || fps_d > 120.0) fps_d = 30.0;
    AVRational fps = av_d2q(fps_d, 1001000);
    if (fps.num <= 0 || fps.den <= 0) fps = (AVRational){30, 1};

    int src_w = dec->dec->width, src_h = dec->dec->height;
    if (src_w <= 0 || src_h <= 0) { set_error("invalid source size %dx%d", src_w, src_h); decoder_close(dec); return AVERROR(EINVAL); }

    AVFilterGraph *filter_graph = NULL;
    AVFilterContext *src_ctx = NULL, *sink_ctx = NULL;
    int out_w = 0, out_h = 0;
    AVRational sink_tb = (AVRational){1, fps.num > 0 ? fps.num : 30};
    int hardware_filter = zero_copy_vaapi || zero_copy_cuda;
    enum AVPixelFormat filter_pix_fmt = hardware_filter ? AV_PIX_FMT_NV12 : AV_PIX_FMT_YUV420P;
    AVBufferRef *filter_device_ctx = hardware_filter ? dec->hw_device_ctx : NULL;
    AVBufferRef *filter_frames_ctx = hardware_filter ? dec->dec->hw_frames_ctx : NULL;
    int ret = init_video_filter(dec, target_width, opts ? opts->crop_width : 0, opts ? opts->crop_height : 0, opts ? opts->crop_x : 0, opts ? opts->crop_y : 0, fps, filter_pix_fmt, zero_copy_vaapi, zero_copy_cuda, filter_device_ctx, filter_frames_ctx, &filter_graph, &src_ctx, &sink_ctx, &out_w, &out_h, &sink_tb);
    if (ret < 0) { decoder_close(dec); return ret; }
    if (out_w <= 0 || out_h <= 0) { set_error("invalid filter output size %dx%d", out_w, out_h); avfilter_graph_free(&filter_graph); decoder_close(dec); return AVERROR(EINVAL); }
    if (out_w % 2) out_w++;
    if (out_h % 2) out_h++;

    AVFormatContext *ofmt = NULL;
    ret = avformat_alloc_output_context2(&ofmt, NULL, format_name, output_path);
    if (ret >= 0 && ofmt && timestamp_offset > 0.0) ofmt->avoid_negative_ts = AVFMT_AVOID_NEG_TS_DISABLED;
    if (ret < 0 || !ofmt) { set_av_error("avformat_alloc_output_context2", ret); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret < 0 ? ret : AVERROR(EINVAL); }

    enum AVCodecID codec_id;
    const AVCodec *encoder = choose_video_encoder(encoder_name, &codec_id);
    if (!encoder) { if (!encoder_name) set_error("no usable video encoder found"); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return AVERROR_ENCODER_NOT_FOUND; }

    AVStream *out_st = avformat_new_stream(ofmt, NULL);
    if (!out_st) { set_error("avformat_new_stream failed"); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return AVERROR(ENOMEM); }

    int want_audio = opts && opts->audio_mode > 0 && dec->audio_stream && dec->adec;
    AVStream *audio_out_st = NULL;
    AVCodecContext *aenc = NULL;
    AVFilterGraph *audio_filter_graph = NULL;
    AVFilterContext *audio_src_ctx = NULL, *audio_sink_ctx = NULL;
    AVRational audio_sink_tb = (AVRational){1, 48000};
    if (want_audio) {
        enum AVCodecID audio_codec_id;
        const AVCodec *audio_encoder = choose_audio_encoder(opts ? opts->audio_codec : NULL, &audio_codec_id);
        if (!audio_encoder) { avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return AVERROR_ENCODER_NOT_FOUND; }
        audio_out_st = avformat_new_stream(ofmt, NULL);
        if (!audio_out_st) { set_error("avformat_new_stream audio failed"); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return AVERROR(ENOMEM); }
        aenc = avcodec_alloc_context3(audio_encoder);
        if (!aenc) { set_error("avcodec_alloc_context3 audio encoder failed"); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return AVERROR(ENOMEM); }
        aenc->codec_id = audio_codec_id;
        aenc->codec_type = AVMEDIA_TYPE_AUDIO;
        aenc->sample_rate = 48000;
        aenc->sample_fmt = AV_SAMPLE_FMT_FLTP;
        av_channel_layout_default(&aenc->ch_layout, opts && opts->audio_channels > 0 ? opts->audio_channels : 2);
        if (aenc->ch_layout.nb_channels <= 0) av_channel_layout_default(&aenc->ch_layout, 2);
        aenc->bit_rate = opts && opts->audio_bitrate > 0 ? opts->audio_bitrate : 128000;
        aenc->time_base = (AVRational){1, aenc->sample_rate};
        if (ofmt->oformat->flags & AVFMT_GLOBALHEADER) aenc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
        AVDictionary *aenc_opts = NULL;
        ret = avcodec_open2(aenc, audio_encoder, &aenc_opts); av_dict_free(&aenc_opts);
        if (ret < 0) { set_av_error("avcodec_open2 audio encoder", ret); avcodec_free_context(&aenc); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret; }
        ret = avcodec_parameters_from_context(audio_out_st->codecpar, aenc);
        if (ret < 0) { set_av_error("avcodec_parameters_from_context audio", ret); avcodec_free_context(&aenc); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret; }
        audio_out_st->time_base = aenc->time_base;
        ret = init_audio_filter(dec, aenc, start_time, duration_limit, &audio_filter_graph, &audio_src_ctx, &audio_sink_ctx, &audio_sink_tb);
        if (ret < 0) { avcodec_free_context(&aenc); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret; }
    }

    AVBufferRef *hw_device_ctx = NULL;
    AVBufferRef *hw_frames_ctx = NULL;
    if (zero_copy_cuda) {
        hw_device_ctx = av_buffer_ref(dec->hw_device_ctx);
        AVBufferRef *sink_hw = av_buffersink_get_hw_frames_ctx(sink_ctx);
        if (sink_hw) hw_frames_ctx = av_buffer_ref(sink_hw);
        if (!hw_frames_ctx) { set_error("CUDA filter did not expose hardware frames"); av_buffer_unref(&hw_device_ctx); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return AVERROR(EINVAL); }
    } else if (use_vaapi) {
        const char *vaapi_device = hardware_device ? hardware_device : "/dev/dri/renderD128";
        if (zero_copy_vaapi) hw_device_ctx = av_buffer_ref(dec->hw_device_ctx);
        else ret = av_hwdevice_ctx_create(&hw_device_ctx, AV_HWDEVICE_TYPE_VAAPI, vaapi_device, NULL, 0);
        if (!zero_copy_vaapi && ret < 0) { set_av_error("av_hwdevice_ctx_create(vaapi)", ret); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret; }
        if (zero_copy_vaapi) { AVBufferRef *sink_hw = av_buffersink_get_hw_frames_ctx(sink_ctx); if (sink_hw) hw_frames_ctx = av_buffer_ref(sink_hw); }
        else {
            hw_frames_ctx = av_hwframe_ctx_alloc(hw_device_ctx);
            if (!hw_frames_ctx) { set_error("av_hwframe_ctx_alloc failed"); av_buffer_unref(&hw_device_ctx); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return AVERROR(ENOMEM); }
            AVHWFramesContext *frames = (AVHWFramesContext *)hw_frames_ctx->data;
            frames->format = AV_PIX_FMT_VAAPI;
            frames->sw_format = AV_PIX_FMT_NV12;
            frames->width = out_w;
            frames->height = out_h;
            frames->initial_pool_size = 20;
            ret = av_hwframe_ctx_init(hw_frames_ctx);
            if (ret < 0) { set_av_error("av_hwframe_ctx_init(vaapi)", ret); av_buffer_unref(&hw_frames_ctx); av_buffer_unref(&hw_device_ctx); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret; }
        }
    }

    AVCodecContext *enc = avcodec_alloc_context3(encoder);
    if (!enc) { set_error("avcodec_alloc_context3 encoder failed"); if (audio_filter_graph) avfilter_graph_free(&audio_filter_graph); if (aenc) avcodec_free_context(&aenc); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return AVERROR(ENOMEM); }

    enc->codec_id = codec_id; enc->codec_type = AVMEDIA_TYPE_VIDEO; enc->width = out_w; enc->height = out_h;
    enc->pix_fmt = zero_copy_cuda ? AV_PIX_FMT_CUDA : (use_vaapi ? AV_PIX_FMT_VAAPI : AV_PIX_FMT_YUV420P); enc->time_base = av_inv_q(fps); enc->framerate = fps;
    if (use_vaapi || zero_copy_cuda) enc->hw_frames_ctx = av_buffer_ref(hw_frames_ctx);
    enc->bit_rate = 0; enc->gop_size = gop_size; enc->max_b_frames = max_b_frames; enc->thread_count = 0;
    if (ofmt->oformat->flags & AVFMT_GLOBALHEADER) enc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;

    AVDictionary *enc_opts = NULL;
    char quality_buf[32]; snprintf(quality_buf, sizeof(quality_buf), "%d", crf);
    if (strcmp(encoder->name, "libx264") == 0 || strcmp(encoder->name, "libx265") == 0) { av_dict_set(&enc_opts, "preset", preset, 0); av_dict_set(&enc_opts, "crf", quality_buf, 0); }
    else if (strstr(encoder->name, "_nvenc")) { if (preset[0]) av_dict_set(&enc_opts, "preset", preset, 0); av_dict_set(&enc_opts, "cq", quality_buf, 0); av_dict_set(&enc_opts, "rc", "vbr", 0); }
    else if (strstr(encoder->name, "_qsv")) { if (preset[0]) av_dict_set(&enc_opts, "preset", preset, 0); av_dict_set(&enc_opts, "global_quality", quality_buf, 0); }
    else if (strstr(encoder->name, "_vaapi")) { av_dict_set(&enc_opts, "global_quality", quality_buf, 0); }
    else if (strstr(encoder->name, "_amf")) { if (preset[0]) av_dict_set(&enc_opts, "quality", preset, 0); av_dict_set(&enc_opts, "qp_i", quality_buf, 0); av_dict_set(&enc_opts, "qp_p", quality_buf, 0); }
    else if (strstr(encoder->name, "_videotoolbox")) { av_dict_set(&enc_opts, "q:v", quality_buf, 0); }
    ret = avcodec_open2(enc, encoder, &enc_opts); av_dict_free(&enc_opts);
    if (ret < 0) { set_av_error("avcodec_open2 encoder", ret); if (audio_filter_graph) avfilter_graph_free(&audio_filter_graph); if (aenc) avcodec_free_context(&aenc); avcodec_free_context(&enc); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret; }

    ret = avcodec_parameters_from_context(out_st->codecpar, enc);
    if (ret < 0) { set_av_error("avcodec_parameters_from_context", ret); if (audio_filter_graph) avfilter_graph_free(&audio_filter_graph); if (aenc) avcodec_free_context(&aenc); avcodec_free_context(&enc); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret; }
    out_st->time_base = enc->time_base;

    int64_t video_pts_offset = 0;
    if (timestamp_offset > 0.0) {
        video_pts_offset = av_rescale_q((int64_t)llround(timestamp_offset * (double)AV_TIME_BASE), AV_TIME_BASE_Q, enc->time_base);
        if (video_pts_offset < 0) video_pts_offset = 0;
    }
    int64_t audio_pts_offset = 0;
    if (want_audio && aenc) {
        if (timestamp_offset > 0.0) {
            audio_pts_offset = av_rescale_q((int64_t)llround(timestamp_offset * (double)AV_TIME_BASE), AV_TIME_BASE_Q, aenc->time_base);
            if (audio_pts_offset < 0) audio_pts_offset = 0;
        }
        // AAC encoders normally emit priming-delay packets with negative PTS.
        // Offset by one encoder frame for independently generated HLS/DASH
        // windows so adjacent segment audio DTS stays monotonic.
        if (duration_limit > 0.0 && aenc->frame_size > 0) audio_pts_offset += aenc->frame_size;
    }

    if (!(ofmt->oformat->flags & AVFMT_NOFILE)) {
        ret = avio_open(&ofmt->pb, output_path, AVIO_FLAG_WRITE);
        if (ret < 0) { set_av_error("avio_open", ret); if (audio_filter_graph) avfilter_graph_free(&audio_filter_graph); if (aenc) avcodec_free_context(&aenc); avcodec_free_context(&enc); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret; }
    }

    AVDictionary *mux_opts = NULL; set_mux_options(&mux_opts, opts);
    ret = avformat_write_header(ofmt, &mux_opts); av_dict_free(&mux_opts);
    if (ret < 0) { set_av_error("avformat_write_header", ret); if (!(ofmt->oformat->flags & AVFMT_NOFILE)) avio_closep(&ofmt->pb); if (audio_filter_graph) avfilter_graph_free(&audio_filter_graph); if (aenc) avcodec_free_context(&aenc); avcodec_free_context(&enc); avformat_free_context(ofmt); avfilter_graph_free(&filter_graph); decoder_close(dec); return ret; }

    AVPacket *enc_pkt = av_packet_alloc();
    AVFrame *filt_frame = av_frame_alloc();
    AVPacket *audio_enc_pkt = NULL;
    AVFrame *audio_filt_frame = NULL;
    if (want_audio) {
        audio_enc_pkt = av_packet_alloc();
        audio_filt_frame = av_frame_alloc();
    }
    if (!enc_pkt || !filt_frame || (want_audio && (!audio_enc_pkt || !audio_filt_frame))) { set_error("encoder packet/filter frame alloc failed"); ret = AVERROR(ENOMEM); goto fail; }

    int64_t first_filter_pts = AV_NOPTS_VALUE, last_out_pts = AV_NOPTS_VALUE, fallback_pts = 0;
    int64_t audio_last_pts = AV_NOPTS_VALUE;
    int stop_decoding = 0;
    int video_done = 0;
    int audio_done = !want_audio;
    while (!stop_decoding && !tc_cancelled(opts) && (ret = av_read_frame(dec->fmt, dec->pkt)) >= 0) {
        if (tc_cancelled(opts)) { av_packet_unref(dec->pkt); ret = AVERROR_EXIT; break; }
        if (want_audio && !audio_done && dec->pkt->stream_index == dec->audio_stream_index) {
            ret = avcodec_send_packet(dec->adec, dec->pkt);
            av_packet_unref(dec->pkt);
            if (ret < 0 && ret != AVERROR(EAGAIN)) { set_av_error("avcodec_send_packet audio", ret); goto fail; }
            while (!tc_cancelled(opts) && (ret = avcodec_receive_frame(dec->adec, dec->audio_frame)) >= 0) {
                if (dec->audio_frame->best_effort_timestamp != AV_NOPTS_VALUE) dec->audio_frame->pts = dec->audio_frame->best_effort_timestamp;
                double aframe_sec = 0.0;
                if (dec->audio_frame->pts != AV_NOPTS_VALUE) aframe_sec = (double)dec->audio_frame->pts * av_q2d(dec->audio_stream->time_base);
                if (duration_limit > 0.0 && aframe_sec > end_time + 0.25) { av_frame_unref(dec->audio_frame); audio_done = 1; break; }
                ret = av_buffersrc_add_frame_flags(audio_src_ctx, dec->audio_frame, AV_BUFFERSRC_FLAG_KEEP_REF);
                av_frame_unref(dec->audio_frame);
                if (ret == AVERROR_EOF) { audio_done = 1; break; }
                if (ret < 0) { set_av_error("audio av_buffersrc_add_frame", ret); goto fail; }
                ret = drain_audio_filter_to_encoder(audio_sink_ctx, audio_sink_tb, ofmt, aenc, audio_out_st, audio_enc_pkt, audio_filt_frame, &audio_last_pts, audio_pts_offset);
                if (ret < 0) goto fail;
            }
            if (ret != AVERROR(EAGAIN) && ret != AVERROR_EOF) { set_av_error("avcodec_receive_frame audio", ret); goto fail; }
            if (tc_cancelled(opts)) { ret = AVERROR_EXIT; break; }
            if (duration_limit > 0.0 && video_done && audio_done) { stop_decoding = 1; break; }
            continue;
        }
        if (dec->pkt->stream_index != dec->stream_index) { av_packet_unref(dec->pkt); continue; }
        if (video_done) { av_packet_unref(dec->pkt); if (audio_done) { stop_decoding = 1; break; } continue; }
        ret = avcodec_send_packet(dec->dec, dec->pkt); av_packet_unref(dec->pkt);
        if (ret < 0 && ret != AVERROR(EAGAIN)) { set_av_error("avcodec_send_packet", ret); goto fail; }
        while (!tc_cancelled(opts) && (ret = avcodec_receive_frame(dec->dec, dec->frame)) >= 0) {
            if (dec->frame->best_effort_timestamp != AV_NOPTS_VALUE) dec->frame->pts = dec->frame->best_effort_timestamp;
            double frame_sec = 0.0;
            if (dec->frame->pts != AV_NOPTS_VALUE) frame_sec = (double)dec->frame->pts * av_q2d(dec->stream->time_base);
            if (start_time > 0.0 && frame_sec + 0.001 < start_time) { av_frame_unref(dec->frame); continue; }
            if (duration_limit > 0.0 && frame_sec >= end_time) { av_frame_unref(dec->frame); video_done = 1; if (audio_done) stop_decoding = 1; ret = AVERROR(EAGAIN); break; }
            ret = av_buffersrc_add_frame_flags(src_ctx, dec->frame, AV_BUFFERSRC_FLAG_KEEP_REF);
            av_frame_unref(dec->frame);
            if (ret < 0) { set_av_error("av_buffersrc_add_frame", ret); goto fail; }
            ret = drain_filter_to_encoder(sink_ctx, sink_tb, ofmt, enc, hw_frames_ctx, out_st, enc_pkt, filt_frame, &first_filter_pts, &last_out_pts, &fallback_pts, video_pts_offset);
            if (ret < 0) goto fail;
        }
        if (ret != AVERROR(EAGAIN) && ret != AVERROR_EOF) { set_av_error("avcodec_receive_frame", ret); goto fail; }
    }
    if (tc_cancelled(opts)) { ret = AVERROR_EXIT; set_error("transcode cancelled"); goto fail; }
    if (ret < 0 && ret != AVERROR_EOF && !stop_decoding) { set_av_error("av_read_frame", ret); goto fail; }

    if (!stop_decoding) ret = avcodec_send_packet(dec->dec, NULL);
    else ret = AVERROR_EOF;
    if (ret >= 0) {
        while (!tc_cancelled(opts) && (ret = avcodec_receive_frame(dec->dec, dec->frame)) >= 0) {
            if (dec->frame->best_effort_timestamp != AV_NOPTS_VALUE) dec->frame->pts = dec->frame->best_effort_timestamp;
            ret = av_buffersrc_add_frame_flags(src_ctx, dec->frame, AV_BUFFERSRC_FLAG_KEEP_REF);
            av_frame_unref(dec->frame);
            if (ret < 0) { set_av_error("flush av_buffersrc_add_frame", ret); goto fail; }
            ret = drain_filter_to_encoder(sink_ctx, sink_tb, ofmt, enc, hw_frames_ctx, out_st, enc_pkt, filt_frame, &first_filter_pts, &last_out_pts, &fallback_pts, video_pts_offset);
            if (ret < 0) goto fail;
        }
        if (ret != AVERROR_EOF && ret != AVERROR(EAGAIN)) { set_av_error("flush avcodec_receive_frame", ret); goto fail; }
    }
    if (tc_cancelled(opts)) { ret = AVERROR_EXIT; set_error("transcode cancelled"); goto fail; }
    ret = av_buffersrc_add_frame_flags(src_ctx, NULL, 0);
    if (ret < 0) { set_av_error("filter flush NULL", ret); goto fail; }
    ret = drain_filter_to_encoder(sink_ctx, sink_tb, ofmt, enc, hw_frames_ctx, out_st, enc_pkt, filt_frame, &first_filter_pts, &last_out_pts, &fallback_pts, video_pts_offset);
    if (ret < 0) goto fail;
    ret = encode_video_frame(ofmt, enc, out_st, enc_pkt, NULL);
    if (ret < 0) goto fail;

    if (want_audio) {
        ret = avcodec_send_packet(dec->adec, NULL);
        if (ret >= 0) {
            while (!tc_cancelled(opts) && (ret = avcodec_receive_frame(dec->adec, dec->audio_frame)) >= 0) {
                if (dec->audio_frame->best_effort_timestamp != AV_NOPTS_VALUE) dec->audio_frame->pts = dec->audio_frame->best_effort_timestamp;
                ret = av_buffersrc_add_frame_flags(audio_src_ctx, dec->audio_frame, AV_BUFFERSRC_FLAG_KEEP_REF);
                av_frame_unref(dec->audio_frame);
                if (ret < 0) { set_av_error("flush audio av_buffersrc_add_frame", ret); goto fail; }
                ret = drain_audio_filter_to_encoder(audio_sink_ctx, audio_sink_tb, ofmt, aenc, audio_out_st, audio_enc_pkt, audio_filt_frame, &audio_last_pts, audio_pts_offset);
                if (ret < 0) goto fail;
            }
            if (ret != AVERROR_EOF && ret != AVERROR(EAGAIN)) { set_av_error("flush avcodec_receive_frame audio", ret); goto fail; }
        }
        if (!audio_done) {
            ret = av_buffersrc_add_frame_flags(audio_src_ctx, NULL, 0);
            if (ret < 0 && ret != AVERROR_EOF) { set_av_error("audio filter flush NULL", ret); goto fail; }
        }
        ret = drain_audio_filter_to_encoder(audio_sink_ctx, audio_sink_tb, ofmt, aenc, audio_out_st, audio_enc_pkt, audio_filt_frame, &audio_last_pts, audio_pts_offset);
        if (ret < 0) goto fail;
        ret = encode_audio_frame(ofmt, aenc, audio_out_st, audio_enc_pkt, NULL);
        if (ret < 0) goto fail;
    }

    ret = av_write_trailer(ofmt);
    if (ret < 0) { set_av_error("av_write_trailer", ret); goto fail; }

    if (enc_pkt) av_packet_free(&enc_pkt); if (filt_frame) av_frame_free(&filt_frame);
    if (audio_enc_pkt) av_packet_free(&audio_enc_pkt); if (audio_filt_frame) av_frame_free(&audio_filt_frame);
    if (audio_filter_graph) avfilter_graph_free(&audio_filter_graph);
    avfilter_graph_free(&filter_graph); if (!(ofmt->oformat->flags & AVFMT_NOFILE)) avio_closep(&ofmt->pb);
    if (aenc) avcodec_free_context(&aenc); avcodec_free_context(&enc); av_buffer_unref(&hw_frames_ctx); av_buffer_unref(&hw_device_ctx); avformat_free_context(ofmt); decoder_close(dec); tc_native_trim(); return 0;

fail:
    if (enc_pkt) av_packet_free(&enc_pkt); if (filt_frame) av_frame_free(&filt_frame);
    if (audio_enc_pkt) av_packet_free(&audio_enc_pkt); if (audio_filt_frame) av_frame_free(&audio_filt_frame);
    if (audio_filter_graph) avfilter_graph_free(&audio_filter_graph);
    if (filter_graph) avfilter_graph_free(&filter_graph); if (ofmt && !(ofmt->oformat->flags & AVFMT_NOFILE)) avio_closep(&ofmt->pb);
    if (aenc) avcodec_free_context(&aenc); if (enc) avcodec_free_context(&enc); av_buffer_unref(&hw_frames_ctx); av_buffer_unref(&hw_device_ctx); if (ofmt) avformat_free_context(ofmt); decoder_close(dec); tc_native_trim();
    return ret < 0 ? ret : AVERROR(EINVAL);
}

static int transcode_audio_segment_opts(TCDecoder *dec, const char *output_path, const TCTranscodeOptions *opts) {
    if (!dec || !dec->adec || !dec->audio_stream || !output_path) {
        set_error("audio transcode requires an audio stream");
        if (dec) decoder_close(dec);
        return AVERROR_STREAM_NOT_FOUND;
    }
    double start_time = opts && opts->start_time > 0.0 ? opts->start_time : 0.0;
    double duration = opts && opts->duration > 0.0 ? opts->duration : 0.0;
    double end_time = duration > 0.0 ? start_time + duration : 0.0;
    double timestamp_offset = opts && opts->timestamp_offset > 0.0 ? opts->timestamp_offset : 0.0;
    int ret = 0;
    AVFormatContext *ofmt = NULL;
    AVCodecContext *aenc = NULL;
    AVFilterGraph *graph = NULL;
    AVFilterContext *src = NULL, *sink = NULL;
    AVPacket *out_pkt = NULL;
    AVFrame *filt = NULL;
    AVRational sink_tb = (AVRational){1, 48000};
    if (start_time > 0.0) {
        int64_t ts = av_rescale_q((int64_t)llround(start_time * AV_TIME_BASE), AV_TIME_BASE_Q, dec->audio_stream->time_base);
        ret = avformat_seek_file(dec->fmt, dec->audio_stream_index, INT64_MIN, ts, ts, AVSEEK_FLAG_BACKWARD);
        if (ret < 0) { set_av_error("audio avformat_seek_file", ret); goto fail; }
        avcodec_flush_buffers(dec->adec);
    }
    ret = avformat_alloc_output_context2(&ofmt, NULL, "mp4", output_path);
    if (ret < 0 || !ofmt) { set_av_error("audio avformat_alloc_output_context2", ret); goto fail; }
    if (timestamp_offset > 0.0) ofmt->avoid_negative_ts = AVFMT_AVOID_NEG_TS_DISABLED;
    enum AVCodecID codec_id;
    const AVCodec *codec = choose_audio_encoder(opts ? opts->audio_codec : NULL, &codec_id);
    if (!codec) { ret = AVERROR_ENCODER_NOT_FOUND; goto fail; }
    AVStream *out_st = avformat_new_stream(ofmt, NULL);
    if (!out_st) { ret = AVERROR(ENOMEM); goto fail; }
    aenc = avcodec_alloc_context3(codec);
    if (!aenc) { ret = AVERROR(ENOMEM); goto fail; }
    aenc->codec_id = codec_id;
    aenc->codec_type = AVMEDIA_TYPE_AUDIO;
    aenc->sample_rate = 48000;
    aenc->sample_fmt = AV_SAMPLE_FMT_FLTP;
    av_channel_layout_default(&aenc->ch_layout, opts && opts->audio_channels > 0 ? opts->audio_channels : 2);
    aenc->bit_rate = opts && opts->audio_bitrate > 0 ? opts->audio_bitrate : 128000;
    aenc->time_base = (AVRational){1, aenc->sample_rate};
    if (ofmt->oformat->flags & AVFMT_GLOBALHEADER) aenc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
    ret = avcodec_open2(aenc, codec, NULL);
    if (ret < 0) { set_av_error("audio avcodec_open2", ret); goto fail; }
    ret = avcodec_parameters_from_context(out_st->codecpar, aenc);
    if (ret < 0) goto fail;
    out_st->time_base = aenc->time_base;
    ret = init_audio_filter(dec, aenc, start_time, duration, &graph, &src, &sink, &sink_tb);
    if (ret < 0) goto fail;
    if (!(ofmt->oformat->flags & AVFMT_NOFILE)) {
        ret = avio_open(&ofmt->pb, output_path, AVIO_FLAG_WRITE);
        if (ret < 0) goto fail;
    }
    AVDictionary *mux = NULL;
    av_dict_set(&mux, "movflags", "frag_keyframe+empty_moov+default_base_moof+omit_tfhd_offset", 0);
    ret = avformat_write_header(ofmt, &mux);
    av_dict_free(&mux);
    if (ret < 0) goto fail;
    out_pkt = av_packet_alloc();
    filt = av_frame_alloc();
    if (!out_pkt || !filt) { ret = AVERROR(ENOMEM); goto fail; }
    int64_t last_pts = AV_NOPTS_VALUE;
    int64_t pts_offset = av_rescale_q((int64_t)llround(timestamp_offset * AV_TIME_BASE), AV_TIME_BASE_Q, aenc->time_base);
    while (!tc_cancelled(opts) && (ret = av_read_frame(dec->fmt, dec->pkt)) >= 0) {
        if (dec->pkt->stream_index != dec->audio_stream_index) { av_packet_unref(dec->pkt); continue; }
        ret = avcodec_send_packet(dec->adec, dec->pkt);
        av_packet_unref(dec->pkt);
        if (ret < 0 && ret != AVERROR(EAGAIN)) goto fail;
        while ((ret = avcodec_receive_frame(dec->adec, dec->audio_frame)) >= 0) {
            if (dec->audio_frame->best_effort_timestamp != AV_NOPTS_VALUE) dec->audio_frame->pts = dec->audio_frame->best_effort_timestamp;
            double sec = dec->audio_frame->pts == AV_NOPTS_VALUE ? 0.0 : dec->audio_frame->pts * av_q2d(dec->audio_stream->time_base);
            if (end_time > 0.0 && sec >= end_time) { av_frame_unref(dec->audio_frame); ret = AVERROR_EOF; break; }
            ret = av_buffersrc_add_frame_flags(src, dec->audio_frame, AV_BUFFERSRC_FLAG_KEEP_REF);
            av_frame_unref(dec->audio_frame);
            if (ret < 0) goto fail;
            ret = drain_audio_filter_to_encoder(sink, sink_tb, ofmt, aenc, out_st, out_pkt, filt, &last_pts, pts_offset);
            if (ret < 0) goto fail;
        }
        if (ret == AVERROR_EOF) break;
        if (ret != AVERROR(EAGAIN) && ret < 0) goto fail;
    }
    if (tc_cancelled(opts)) { ret = AVERROR_EXIT; set_error("audio transcode cancelled"); goto fail; }
    ret = av_buffersrc_add_frame_flags(src, NULL, 0);
    if (ret < 0 && ret != AVERROR_EOF) goto fail;
    ret = drain_audio_filter_to_encoder(sink, sink_tb, ofmt, aenc, out_st, out_pkt, filt, &last_pts, pts_offset);
    if (ret < 0) goto fail;
    ret = encode_audio_frame(ofmt, aenc, out_st, out_pkt, NULL);
    if (ret < 0) goto fail;
    ret = av_write_trailer(ofmt);
fail:
    if (filt) av_frame_free(&filt);
    if (out_pkt) av_packet_free(&out_pkt);
    if (graph) avfilter_graph_free(&graph);
    if (aenc) avcodec_free_context(&aenc);
    if (ofmt && !(ofmt->oformat->flags & AVFMT_NOFILE)) avio_closep(&ofmt->pb);
    if (ofmt) avformat_free_context(ofmt);
    decoder_close(dec);
    return ret < 0 && ret != AVERROR_EOF ? ret : 0;
}

int tc_transcode_fmp4_segment_audio(const char *input_path, const char *output_path, const TCTranscodeOptions *opts) {
    if (!input_path || !output_path) return AVERROR(EINVAL);
    TCDecoder *dec = decoder_open(input_path, NULL, NULL, 0);
    if (!dec) return AVERROR(EINVAL);
    return transcode_audio_segment_opts(dec, output_path, opts);
}

int tc_transcode_video(const char *input_path, const char *output_path, const TCTranscodeOptions *opts) {
    if (!input_path || !output_path) { set_error("tc_transcode_video: nil path"); return AVERROR(EINVAL); }
    TCDecoder *dec = decoder_open(input_path, opts ? opts->encoder_name : NULL, opts ? opts->hardware_device : NULL, opts && opts->hardware_decode);
    if (!dec) return AVERROR(EINVAL);
    return transcode_decoder_to_video_opts(dec, output_path, opts);
}

int tc_transcode_hls_video(const char *input_path, const char *playlist_path, const char *segment_filename, const TCTranscodeOptions *opts) {
    if (!input_path || !playlist_path || !segment_filename) { set_error("tc_transcode_hls_video: nil path"); return AVERROR(EINVAL); }
    TCTranscodeOptions local;
    memset(&local, 0, sizeof(local));
    if (opts) local = *opts;
    local.format = "hls";
    local.segment_filename = segment_filename;
    if (local.hls_time <= 0.0) local.hls_time = 4.0;
    if (!local.hls_playlist_type) local.hls_playlist_type = "vod";
    if (!local.hls_segment_type) local.hls_segment_type = "mpegts";
    TCDecoder *dec = decoder_open(input_path, local.encoder_name, local.hardware_device, local.hardware_decode);
    if (!dec) return AVERROR(EINVAL);
    return transcode_decoder_to_video_opts(dec, playlist_path, &local);
}

int tc_transcode_segment_video(const char *input_path, const char *output_path, const TCTranscodeOptions *opts) {
    if (!input_path || !output_path) { set_error("tc_transcode_segment_video: nil path"); return AVERROR(EINVAL); }
    TCTranscodeOptions local;
    memset(&local, 0, sizeof(local));
    if (opts) local = *opts;
    local.format = "mpegts";
    TCDecoder *dec = decoder_open(input_path, local.encoder_name, local.hardware_device, local.hardware_decode);
    if (!dec) return AVERROR(EINVAL);
    return transcode_decoder_to_video_opts(dec, output_path, &local);
}

int tc_transcode_fmp4_segment_video(const char *input_path, const char *output_path, const TCTranscodeOptions *opts) {
    if (!input_path || !output_path) { set_error("tc_transcode_fmp4_segment_video: nil path"); return AVERROR(EINVAL); }
    TCTranscodeOptions local;
    memset(&local, 0, sizeof(local));
    if (opts) local = *opts;
    local.format = "mp4";
    local.faststart = 0;
    TCDecoder *dec = decoder_open(input_path, local.encoder_name, local.hardware_device, local.hardware_decode);
    if (!dec) return AVERROR(EINVAL);
    return transcode_decoder_to_video_opts(dec, output_path, &local);
}

void *tc_fmp4_video_decoder_open(const char *input_path, const char *encoder_name, const char *hardware_device, int hardware_decode) {
    if (!input_path) { set_error("tc_fmp4_video_decoder_open: nil path"); return NULL; }
    TCDecoder *dec = decoder_open(input_path, encoder_name, hardware_device, hardware_decode);
    if (!dec) return NULL;
    dec->persistent = 1;
    return dec;
}

int tc_fmp4_video_decoder_transcode(void *decoder, const char *output_path, const TCTranscodeOptions *opts) {
    TCDecoder *dec = (TCDecoder *)decoder;
    if (!dec || !output_path) { set_error("tc_fmp4_video_decoder_transcode: nil input"); return AVERROR(EINVAL); }
    TCTranscodeOptions local;
    memset(&local, 0, sizeof(local));
    if (opts) local = *opts;
    local.format = "mp4";
    local.faststart = 0;
    return transcode_decoder_to_video_opts(dec, output_path, &local);
}

void tc_fmp4_video_decoder_close(void *decoder) {
    decoder_destroy((TCDecoder *)decoder);
}

int tc_encoder_available(const char *encoder_name) {
    if (!encoder_name || !encoder_name[0]) return 0;
    ffmpeg_init();
    return avcodec_find_encoder_by_name(encoder_name) ? 1 : 0;
}

int tc_transcode_dash_video(const char *input_path, const char *mpd_path, const TCTranscodeOptions *opts) {
    if (!input_path || !mpd_path) { set_error("tc_transcode_dash_video: nil path"); return AVERROR(EINVAL); }
    TCTranscodeOptions local;
    memset(&local, 0, sizeof(local));
    if (opts) local = *opts;
    local.format = "dash";
    TCDecoder *dec = decoder_open(input_path, local.encoder_name, local.hardware_device, local.hardware_decode);
    if (!dec) return AVERROR(EINVAL);
    return transcode_decoder_to_video_opts(dec, mpd_path, &local);
}

#include <libavutil/bprint.h>
#include <libavutil/hwcontext.h>
#include <libavutil/version.h>

static void json_comma(AVBPrint *b, int *first) {
    if (!*first) av_bprintf(b, ",");
    *first = 0;
}

static void json_string(AVBPrint *b, const char *s) {
    av_bprintf(b, "\"");
    if (s) {
        for (const unsigned char *p = (const unsigned char *)s; *p; p++) {
            switch (*p) {
                case '\\': av_bprintf(b, "\\\\"); break;
                case '"': av_bprintf(b, "\\\""); break;
                case '\n': av_bprintf(b, "\\n"); break;
                case '\r': av_bprintf(b, "\\r"); break;
                case '\t': av_bprintf(b, "\\t"); break;
                default:
                    if (*p < 0x20) av_bprintf(b, "\\u%04x", *p);
                    else av_bprintf(b, "%c", *p);
            }
        }
    }
    av_bprintf(b, "\"");
}

char *tc_runtime_capabilities_json(void) {
    ffmpeg_init();
    AVBPrint b;
    av_bprint_init(&b, 0, AV_BPRINT_SIZE_UNLIMITED);
    av_bprintf(&b, "{");
    av_bprintf(&b, "\"ffmpeg_version\":"); json_string(&b, av_version_info());
    av_bprintf(&b, ",\"libavcodec_version\":%u", avcodec_version());
    av_bprintf(&b, ",\"libavformat_version\":%u", avformat_version());
    av_bprintf(&b, ",\"libavutil_version\":%u", avutil_version());

    av_bprintf(&b, ",\"hardware_device_types\":[");
    int first = 1;
    enum AVHWDeviceType hw = AV_HWDEVICE_TYPE_NONE;
    while ((hw = av_hwdevice_iterate_types(hw)) != AV_HWDEVICE_TYPE_NONE) {
        const char *name = av_hwdevice_get_type_name(hw);
        if (!name) continue;
        json_comma(&b, &first);
        json_string(&b, name);
    }
    av_bprintf(&b, "]");

    av_bprintf(&b, ",\"video_encoders\":[");
    void *it = NULL;
    const AVCodec *codec = NULL;
    first = 1;
    while ((codec = av_codec_iterate(&it))) {
        if (!av_codec_is_encoder(codec) || codec->type != AVMEDIA_TYPE_VIDEO) continue;
        json_comma(&b, &first);
        json_string(&b, codec->name);
    }
    av_bprintf(&b, "]");

    av_bprintf(&b, ",\"video_decoders\":[");
    it = NULL;
    first = 1;
    while ((codec = av_codec_iterate(&it))) {
        if (!av_codec_is_decoder(codec) || codec->type != AVMEDIA_TYPE_VIDEO) continue;
        json_comma(&b, &first);
        json_string(&b, codec->name);
    }
    av_bprintf(&b, "]");

    av_bprintf(&b, ",\"audio_encoders\":[");
    it = NULL;
    first = 1;
    while ((codec = av_codec_iterate(&it))) {
        if (!av_codec_is_encoder(codec) || codec->type != AVMEDIA_TYPE_AUDIO) continue;
        json_comma(&b, &first);
        json_string(&b, codec->name);
    }
    av_bprintf(&b, "]");

    av_bprintf(&b, ",\"audio_decoders\":[");
    it = NULL;
    first = 1;
    while ((codec = av_codec_iterate(&it))) {
        if (!av_codec_is_decoder(codec) || codec->type != AVMEDIA_TYPE_AUDIO) continue;
        json_comma(&b, &first);
        json_string(&b, codec->name);
    }
    av_bprintf(&b, "]");

    av_bprintf(&b, ",\"muxers\":[");
    void *ofmt_it = NULL;
    const AVOutputFormat *ofmt = NULL;
    first = 1;
    while ((ofmt = av_muxer_iterate(&ofmt_it))) {
        if (!ofmt->name) continue;
        json_comma(&b, &first);
        json_string(&b, ofmt->name);
    }
    av_bprintf(&b, "]");

    av_bprintf(&b, ",\"demuxers\":[");
    void *ifmt_it = NULL;
    const AVInputFormat *ifmt = NULL;
    first = 1;
    while ((ifmt = av_demuxer_iterate(&ifmt_it))) {
        if (!ifmt->name) continue;
        json_comma(&b, &first);
        json_string(&b, ifmt->name);
    }
    av_bprintf(&b, "]");

    av_bprintf(&b, "}");
    char *out = NULL;
    if (av_bprint_finalize(&b, &out) < 0) return NULL;
    return out;
}
void tc_free(void *p) { av_free(p); }
