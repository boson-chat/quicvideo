//go:build linux && cgo

// Capture on Linux: libavdevice (V4L2 cameras, x11grab) and PipeWire
// (screens/windows from the xdg-desktop-portal ScreenCast session).
#include <dirent.h>
#include <fcntl.h>
#include <libavcodec/avcodec.h>
#include <libavdevice/avdevice.h>
#include <libavformat/avformat.h>
#include <libavutil/time.h>
#include <linux/videodev2.h>
#include <pipewire/pipewire.h>
#include <pthread.h>
#include <spa/param/video/format-utils.h>
#include <spa/param/video/raw.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <unistd.h>
#include "media_linux.h"

extern void qvGoCapFrame(uintptr_t handle, AVFrame *f, int64_t us);
extern void qvGoCapStopped(uintptr_t handle, char *err);

static int64_t nowUS(void) { return av_gettime(); } // wall clock µs

static char *errf2(const char *what, int code) {
    char buf[256], av[128];
    av_strerror(code, av, sizeof av);
    snprintf(buf, sizeof buf, "%s: %s", what, av);
    return strdup(buf);
}

// ---------------------------------------------------------------- V4L2 list

char *qv_v4l2_list(void) {
    size_t cap = 4096, n = 0;
    char *out = calloc(1, cap);
    DIR *d = opendir("/dev");
    if (!d) return out;
    struct dirent *e;
    while ((e = readdir(d))) {
        if (strncmp(e->d_name, "video", 5) != 0) continue;
        char path[300];
        snprintf(path, sizeof path, "/dev/%s", e->d_name);
        int fd = open(path, O_RDWR | O_NONBLOCK);
        if (fd < 0) continue;
        struct v4l2_capability c;
        memset(&c, 0, sizeof c);
        if (ioctl(fd, VIDIOC_QUERYCAP, &c) == 0) {
            uint32_t caps = (c.capabilities & V4L2_CAP_DEVICE_CAPS) ? c.device_caps : c.capabilities;
            if (caps & V4L2_CAP_VIDEO_CAPTURE) { // skip metadata nodes
                char line[512];
                int l = snprintf(line, sizeof line, "%s\t%s\t%s\n", path, (char *)c.card, (char *)c.bus_info);
                if (n + l + 1 > cap) out = realloc(out, cap *= 2);
                memcpy(out + n, line, l + 1);
                n += l;
            }
        }
        close(fd);
    }
    closedir(d);
    return out;
}

// ---------------------------------------------------------------- libavdevice

struct qv_avcap {
    AVFormatContext *fmt;
    AVCodecContext *dec;
    int stream;
    uintptr_t handle;
    pthread_t thread;
    volatile int stop;
};

// ownedFrame: the frame to hand to Go. For raw device formats (NV12,
// YUYV...) the rawvideo decoder's frames point into the V4L2 driver's
// mmap'd buffers; Go keeps pictures (the publisher holds the last one for
// keyframe re-encodes), which starved the driver of buffers, and after
// close they were unmapped under us ("Some buffers are still owned by the
// caller on close", then VIDIOC_QBUF on a closed fd). Those are copied into
// frames we own; decoded MJPEG frames already are.
static AVFrame *ownedFrame(struct qv_avcap *c, AVFrame *f) {
    if (c->dec->codec_id != AV_CODEC_ID_RAWVIDEO) return av_frame_clone(f);
    AVFrame *out = av_frame_alloc();
    if (!out) return NULL;
    out->format = f->format;
    out->width = f->width;
    out->height = f->height;
    if (av_frame_get_buffer(out, 0) < 0 || av_frame_copy(out, f) < 0 || av_frame_copy_props(out, f) < 0) {
        av_frame_free(&out);
        return NULL;
    }
    return out;
}

static void *avcapLoop(void *arg) {
    struct qv_avcap *c = arg;
    AVPacket *pkt = av_packet_alloc();
    AVFrame *f = av_frame_alloc();
    char *err = NULL;
    while (!c->stop) {
        int r = av_read_frame(c->fmt, pkt);
        if (r == AVERROR(EAGAIN)) {
            av_usleep(2000);
            continue;
        }
        if (r < 0) {
            err = errf2("capture ended", r);
            break;
        }
        if (pkt->stream_index == c->stream && avcodec_send_packet(c->dec, pkt) >= 0) {
            while (avcodec_receive_frame(c->dec, f) >= 0) {
                AVFrame *out = ownedFrame(c, f);
                av_frame_unref(f);
                if (out) qvGoCapFrame(c->handle, out, nowUS());
            }
        }
        av_packet_unref(pkt);
    }
    av_packet_free(&pkt);
    av_frame_free(&f);
    if (!c->stop) qvGoCapStopped(c->handle, err); // Go frees err
    else free(err);
    return NULL;
}

// openInput opens the device non-blocking (AVFMT_FLAG_NONBLOCK: V4L2 opens
// it O_NONBLOCK), so av_read_frame returns EAGAIN instead of sleeping in
// VIDIOC_DQBUF. A stalled or wedged camera then can't block Stop's join:
// the loop sees the stop flag within a few ms and the device is released
// (STREAMOFF, munmap, close) by avformat_close_input.
static int openInput(AVFormatContext **fmt, const char *url, const AVInputFormat *ifmt, AVDictionary **opts) {
    *fmt = avformat_alloc_context();
    if (!*fmt) return AVERROR(ENOMEM);
    (*fmt)->flags |= AVFMT_FLAG_NONBLOCK;
    return avformat_open_input(fmt, url, ifmt, opts); // frees *fmt on failure
}

qv_avcap *qv_avcap_start(const char *fmtName, const char *url, int w, int h, int fps, uintptr_t handle, char **err) {
    avdevice_register_all();
    const AVInputFormat *ifmt = av_find_input_format(fmtName);
    if (!ifmt) {
        *err = strdup("libavdevice lacks this input format");
        return NULL;
    }
    AVDictionary *opts = NULL;
    char buf[64];
    if (w > 0 && h > 0) {
        snprintf(buf, sizeof buf, "%dx%d", w, h);
        av_dict_set(&opts, "video_size", buf, 0);
    }
    snprintf(buf, sizeof buf, "%d", fps);
    av_dict_set(&opts, "framerate", buf, 0);
    if (strcmp(fmtName, "video4linux2") == 0) {
        av_dict_set(&opts, "input_format", "mjpeg", 0); // most webcams only reach 30 fps at HD in MJPEG
    } else {
        av_dict_set(&opts, "draw_mouse", "1", 0);
    }
    struct qv_avcap *c = calloc(1, sizeof *c);
    c->handle = handle;
    int r = openInput(&c->fmt, url, ifmt, &opts);
    if (r < 0 && strcmp(fmtName, "video4linux2") == 0) {
        // No MJPEG at that size: let the driver pick a raw format.
        av_dict_set(&opts, "input_format", NULL, 0);
        r = openInput(&c->fmt, url, ifmt, &opts);
    }
    av_dict_free(&opts);
    if (r < 0) {
        *err = errf2("opening the device failed", r);
        free(c);
        return NULL;
    }
    avformat_find_stream_info(c->fmt, NULL);
    c->stream = av_find_best_stream(c->fmt, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    if (c->stream < 0) {
        *err = strdup("no video stream");
        avformat_close_input(&c->fmt);
        free(c);
        return NULL;
    }
    AVCodecParameters *par = c->fmt->streams[c->stream]->codecpar;
    const AVCodec *dec = avcodec_find_decoder(par->codec_id);
    c->dec = avcodec_alloc_context3(dec);
    avcodec_parameters_to_context(c->dec, par);
    if (!dec || avcodec_open2(c->dec, dec, NULL) < 0) {
        *err = strdup("no decoder for the device's format");
        avcodec_free_context(&c->dec);
        avformat_close_input(&c->fmt);
        free(c);
        return NULL;
    }
    pthread_create(&c->thread, NULL, avcapLoop, c);
    return c;
}

void qv_avcap_stop(qv_avcap *c) {
    if (!c) return;
    c->stop = 1;
    pthread_join(c->thread, NULL);
    avcodec_free_context(&c->dec);
    avformat_close_input(&c->fmt);
    free(c);
}

// ---------------------------------------------------------------- PipeWire

struct qv_pwcap {
    struct pw_thread_loop *loop;
    struct pw_context *context;
    struct pw_core *core;
    struct pw_stream *stream;
    struct spa_hook listener;
    struct spa_video_info_raw info;
    int have_info;
    uintptr_t handle;
    int max_w;
    int64_t last_us;
    int64_t min_interval_us;
};

static enum AVPixelFormat avFormat(uint32_t spa) {
    switch (spa) {
    case SPA_VIDEO_FORMAT_BGRx:
        return AV_PIX_FMT_BGR0;
    case SPA_VIDEO_FORMAT_BGRA:
        return AV_PIX_FMT_BGRA;
    case SPA_VIDEO_FORMAT_RGBx:
        return AV_PIX_FMT_RGB0;
    case SPA_VIDEO_FORMAT_RGBA:
        return AV_PIX_FMT_RGBA;
    case SPA_VIDEO_FORMAT_xRGB:
        return AV_PIX_FMT_0RGB;
    case SPA_VIDEO_FORMAT_xBGR:
        return AV_PIX_FMT_0BGR;
    }
    return AV_PIX_FMT_NONE;
}

static void onParamChanged(void *data, uint32_t id, const struct spa_pod *param) {
    struct qv_pwcap *c = data;
    if (param == NULL || id != SPA_PARAM_Format) return;
    struct spa_video_info info;
    if (spa_format_parse(param, &info.media_type, &info.media_subtype) < 0 ||
        info.media_type != SPA_MEDIA_TYPE_video || info.media_subtype != SPA_MEDIA_SUBTYPE_raw)
        return;
    spa_format_video_raw_parse(param, &c->info);
    c->have_info = 1;
    // Ask for buffers we can map (shared memory), not DMA-BUFs.
    uint8_t buf[1024];
    struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buf, sizeof buf);
    const struct spa_pod *params[1];
    params[0] = spa_pod_builder_add_object(
        &b, SPA_TYPE_OBJECT_ParamBuffers, SPA_PARAM_Buffers, SPA_PARAM_BUFFERS_buffers,
        SPA_POD_CHOICE_RANGE_Int(4, 2, 8), SPA_PARAM_BUFFERS_dataType,
        SPA_POD_CHOICE_FLAGS_Int((1 << SPA_DATA_MemPtr) | (1 << SPA_DATA_MemFd)));
    pw_stream_update_params(c->stream, params, 1);
}

static void onProcess(void *data) {
    struct qv_pwcap *c = data;
    struct pw_buffer *b = pw_stream_dequeue_buffer(c->stream);
    if (!b) return;
    struct spa_buffer *buf = b->buffer;
    struct spa_data *d = &buf->datas[0];
    int64_t now = nowUS();
    // Frames only come when something changed (damage); still cap the rate.
    if (c->have_info && d->data && d->chunk->size > 0 && now - c->last_us >= c->min_interval_us) {
        enum AVPixelFormat fmt = avFormat(c->info.format);
        int w = c->info.size.width, h = c->info.size.height;
        if (fmt != AV_PIX_FMT_NONE && w > 0 && h > 0) {
            AVFrame *f = av_frame_alloc();
            f->format = fmt;
            f->width = w;
            f->height = h;
            if (av_frame_get_buffer(f, 32) == 0) {
                int stride = d->chunk->stride ? d->chunk->stride : w * 4;
                const uint8_t *src = (const uint8_t *)d->data + d->chunk->offset;
                for (int y = 0; y < h; y++) memcpy(f->data[0] + y * f->linesize[0], src + y * stride, (size_t)w * 4);
                c->last_us = now;
                qvGoCapFrame(c->handle, f, now);
            } else {
                av_frame_free(&f);
            }
        }
    }
    pw_stream_queue_buffer(c->stream, b);
}

static void onStateChanged(void *data, enum pw_stream_state old, enum pw_stream_state state, const char *error) {
    struct qv_pwcap *c = data;
    if (state == PW_STREAM_STATE_ERROR || state == PW_STREAM_STATE_UNCONNECTED) {
        qvGoCapStopped(c->handle, error ? strdup(error) : strdup("screen capture stream closed"));
    }
}

static const struct pw_stream_events streamEvents = {
    PW_VERSION_STREAM_EVENTS,
    .state_changed = onStateChanged,
    .param_changed = onParamChanged,
    .process = onProcess,
};

qv_pwcap *qv_pwcap_start(int fd, uint32_t node, int max_w, int fps, uintptr_t handle, char **err) {
    pw_init(NULL, NULL);
    struct qv_pwcap *c = calloc(1, sizeof *c);
    c->handle = handle;
    c->max_w = max_w;
    c->min_interval_us = 1000000 / (fps > 0 ? fps : 15) * 85 / 100;
    c->loop = pw_thread_loop_new("quicvideo-screen", NULL);
    c->context = pw_context_new(pw_thread_loop_get_loop(c->loop), NULL, 0);
    if (pw_thread_loop_start(c->loop) < 0) {
        *err = strdup("pipewire: cannot start loop");
        goto fail;
    }
    pw_thread_loop_lock(c->loop);
    c->core = pw_context_connect_fd(c->context, fd, NULL, 0); // fd from OpenPipeWireRemote (owned now)
    if (!c->core) {
        pw_thread_loop_unlock(c->loop);
        *err = strdup("pipewire: cannot connect to the portal's remote");
        goto fail;
    }
    c->stream = pw_stream_new(c->core, "quicvideo screen",
                              pw_properties_new(PW_KEY_MEDIA_TYPE, "Video", PW_KEY_MEDIA_CATEGORY, "Capture",
                                                PW_KEY_MEDIA_ROLE, "Screen", NULL));
    pw_stream_add_listener(c->stream, &c->listener, &streamEvents, c);
    uint8_t buf[1024];
    struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buf, sizeof buf);
    const struct spa_pod *params[1];
    struct spa_rectangle def = SPA_RECTANGLE(1920, 1080), mn = SPA_RECTANGLE(1, 1), mx = SPA_RECTANGLE(8192, 8192);
    struct spa_fraction fdef = SPA_FRACTION(fps, 1), fmin = SPA_FRACTION(0, 1), fmax = SPA_FRACTION(144, 1);
    params[0] = spa_pod_builder_add_object(
        &b, SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat, SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
        SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw), SPA_FORMAT_VIDEO_format,
        SPA_POD_CHOICE_ENUM_Id(7, SPA_VIDEO_FORMAT_BGRx, SPA_VIDEO_FORMAT_BGRx, SPA_VIDEO_FORMAT_BGRA,
                               SPA_VIDEO_FORMAT_RGBx, SPA_VIDEO_FORMAT_RGBA, SPA_VIDEO_FORMAT_xRGB,
                               SPA_VIDEO_FORMAT_xBGR),
        SPA_FORMAT_VIDEO_size, SPA_POD_CHOICE_RANGE_Rectangle(&def, &mn, &mx), SPA_FORMAT_VIDEO_framerate,
        SPA_POD_CHOICE_RANGE_Fraction(&fdef, &fmin, &fmax));
    if (pw_stream_connect(c->stream, PW_DIRECTION_INPUT, node,
                          PW_STREAM_FLAG_AUTOCONNECT | PW_STREAM_FLAG_MAP_BUFFERS, params, 1) < 0) {
        pw_thread_loop_unlock(c->loop);
        *err = strdup("pipewire: cannot connect the stream");
        goto fail;
    }
    pw_thread_loop_unlock(c->loop);
    return c;
fail:
    qv_pwcap_stop(c);
    return NULL;
}

void qv_pwcap_stop(qv_pwcap *c) {
    if (!c) return;
    if (c->loop) pw_thread_loop_stop(c->loop);
    if (c->stream) pw_stream_destroy(c->stream);
    if (c->core) pw_core_disconnect(c->core);
    if (c->context) pw_context_destroy(c->context);
    if (c->loop) pw_thread_loop_destroy(c->loop);
    free(c);
}
