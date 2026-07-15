#pragma once
#include <stdint.h>

typedef struct TCInfo {
    double duration;
    int width;
    int height;
    double fps;
    int audio_streams;
    int has_audio;
} TCInfo;

typedef struct TCTranscodeOptions {
    int target_width;
    double target_fps;
    int crf;
    int gop_size;
    int max_b_frames;
    int faststart;
    const char *preset;
    const char *encoder_name;
    const char *hardware_device;
    int hardware_decode;
    const char *format;
    const char *segment_filename;
    double hls_time;
    int hls_list_size;
    const char *hls_playlist_type;
    const char *hls_segment_type;
    int audio_mode;
    int audio_stream;
    int audio_bitrate;
    int audio_channels;
    const char *audio_codec;
    double start_time;
    double duration;
    double timestamp_offset;
    volatile int *cancel_flag;
} TCTranscodeOptions;

typedef struct TCCodecInfo {
    int media_type;
    int codec_id;
    int profile;
    int level;
    int sample_rate;
    int channels;
    char codec_name[64];
    char codec_string[128];
} TCCodecInfo;

const char *tc_last_error(void);
int tc_probe(const char *input_path, TCInfo *info);
int tc_probe_codec(const char *input_path, int media_type, TCCodecInfo *info);
int tc_transcode_video(const char *input_path, const char *output_path, const TCTranscodeOptions *opts);
int tc_transcode_hls_video(const char *input_path, const char *playlist_path, const char *segment_filename, const TCTranscodeOptions *opts);
int tc_transcode_segment_video(const char *input_path, const char *output_path, const TCTranscodeOptions *opts);
int tc_transcode_fmp4_segment_video(const char *input_path, const char *output_path, const TCTranscodeOptions *opts);
int tc_transcode_fmp4_segment_audio(const char *input_path, const char *output_path, const TCTranscodeOptions *opts);
int tc_encoder_available(const char *encoder_name);
int tc_transcode_dash_video(const char *input_path, const char *mpd_path, const TCTranscodeOptions *opts);
volatile int *tc_cancel_alloc(void);
void tc_cancel_set(volatile int *flag);
void tc_cancel_free(volatile int *flag);
char *tc_runtime_capabilities_json(void);
void tc_free(void *p);
