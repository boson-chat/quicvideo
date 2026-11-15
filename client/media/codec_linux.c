//go:build linux && cgo

// H.264 encode (NVENC / VA-API / openh264 / opt-in x264) and decode
// (libavcodec) for package media on Linux.
#include <libavcodec/avcodec.h>
#include <libavutil/hwcontext.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
#include <libswscale/swscale.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <wels/codec_api.h>
#include "media_linux.h"

extern void qvGoLencOutput(uintptr_t handle, uint8_t *data, int len, int key, int64_t us);
extern void qvGoLdecOutput(uintptr_t handle, AVFrame *f, int64_t us);

static char *errf(const char *what, int code) {
    char buf[256], av[128];
    av_strerror(code, av, sizeof av);
    snprintf(buf, sizeof buf, "%s: %s (%d)", what, av, code);
    return strdup(buf);
}

// ---------------------------------------------------------------- frames

AVFrame *qv_frame_new_bgra(int w, int h) {
    AVFrame *f = av_frame_alloc();
    f->format = AV_PIX_FMT_BGRA;
    f->width = w;
    f->height = h;
    if (av_frame_get_buffer(f, 32) < 0) {
        av_frame_free(&f);
        return NULL;
    }
    return f;
}

void qv_frame_free(AVFrame *f) { av_frame_free(&f); }
int qv_frame_w(AVFrame *f) { return f->width; }
int qv_frame_h(AVFrame *f) { return f->height; }
int qv_frame_format(AVFrame *f) { return f->format; }
uint8_t *qv_frame_data(AVFrame *f, int plane, int *linesize) {
    *linesize = f->linesize[plane];
    return f->data[plane];
}

// convert scales src into a new frame of format/size (bilinear).
static AVFrame *convert(AVFrame *src, enum AVPixelFormat fmt, int w, int h) {
    AVFrame *dst = av_frame_alloc();
    dst->format = fmt;
    dst->width = w;
    dst->height = h;
    if (av_frame_get_buffer(dst, 32) < 0) goto fail;
    struct SwsContext *sws = sws_getContext(src->width, src->height, src->format, w, h, fmt, SWS_BILINEAR, NULL,
                                            NULL, NULL);
    if (!sws) goto fail;
    sws_scale(sws, (const uint8_t *const *)src->data, src->linesize, 0, src->height, dst->data, dst->linesize);
    sws_freeContext(sws);
    return dst;
fail:
    av_frame_free(&dst);
    return NULL;
}

AVFrame *qv_frame_to_rgba(AVFrame *src, int w, int h) { return convert(src, AV_PIX_FMT_RGBA, w, h); }

// ---------------------------------------------------------------- encoder

struct qv_lenc {
    int backend;
    int w, h;
    uintptr_t handle;
    // libavcodec backends
    AVCodecContext *ctx;
    AVBufferRef *hwdev;
    AVPacket *pkt;
    struct SwsContext *sws;
    int swsSrcFmt, swsSrcW, swsSrcH;
    AVFrame *conv; // reusable converted frame (NV12 / YUV420P)
    // openh264
    ISVCEncoder *oh;
};

static int ensureConv(qv_lenc *e, AVFrame *src, enum AVPixelFormat fmt) {
    if (!e->conv) {
        e->conv = av_frame_alloc();
        e->conv->format = fmt;
        e->conv->width = e->w;
        e->conv->height = e->h;
        if (av_frame_get_buffer(e->conv, 32) < 0) return -1;
    }
    if (!e->sws || e->swsSrcFmt != src->format || e->swsSrcW != src->width || e->swsSrcH != src->height) {
        sws_freeContext(e->sws);
        e->sws = sws_getContext(src->width, src->height, src->format, e->w, e->h, fmt, SWS_BILINEAR, NULL, NULL, NULL);
        e->swsSrcFmt = src->format;
        e->swsSrcW = src->width;
        e->swsSrcH = src->height;
        if (!e->sws) return -1;
    }
    if (av_frame_make_writable(e->conv) < 0) return -1;
    sws_scale(e->sws, (const uint8_t *const *)src->data, src->linesize, 0, src->height, e->conv->data,
              e->conv->linesize);
    return 0;
}

static int openAV(qv_lenc *e, const char *codec, int fps, int bitrate, int gop, int screen, char **err) {
    const AVCodec *c = avcodec_find_encoder_by_name(codec);
    if (!c) {
        *err = strdup("encoder not available in this libavcodec");
        return -1;
    }
    AVCodecContext *ctx = avcodec_alloc_context3(c);
    ctx->width = e->w;
    ctx->height = e->h;
    ctx->time_base = (AVRational){1, 1000000};
    ctx->framerate = (AVRational){fps, 1};
    ctx->gop_size = gop;
    ctx->max_b_frames = 0;
    ctx->bit_rate = bitrate;
    ctx->rc_max_rate = bitrate * 3 / 2;
    ctx->rc_buffer_size = bitrate; // ~1 s VBV: keyframes fit, bursts stay bounded
    ctx->flags |= AV_CODEC_FLAG_LOW_DELAY;
    ctx->pix_fmt = AV_PIX_FMT_NV12;
    if (e->backend == QV_ENC_NVENC) {
        av_opt_set(ctx->priv_data, "preset", "p2", 0);
        av_opt_set(ctx->priv_data, "tune", "ull", 0);
        av_opt_set(ctx->priv_data, "rc", "cbr", 0);
        av_opt_set_int(ctx->priv_data, "zerolatency", 1, 0);
        av_opt_set_int(ctx->priv_data, "delay", 0, 0);
        av_opt_set_int(ctx->priv_data, "forced-idr", 1, 0);
        av_opt_set(ctx->priv_data, "profile", "high", 0);
    } else if (e->backend == QV_ENC_VAAPI) {
        int r = av_hwdevice_ctx_create(&e->hwdev, AV_HWDEVICE_TYPE_VAAPI, NULL, NULL, 0);
        if (r < 0) {
            *err = errf("no VA-API device", r);
            avcodec_free_context(&ctx);
            return -1;
        }
        AVBufferRef *frames = av_hwframe_ctx_alloc(e->hwdev);
        AVHWFramesContext *fc = (AVHWFramesContext *)frames->data;
        fc->format = AV_PIX_FMT_VAAPI;
        fc->sw_format = AV_PIX_FMT_NV12;
        fc->width = e->w;
        fc->height = e->h;
        fc->initial_pool_size = 8;
        if ((r = av_hwframe_ctx_init(frames)) < 0) {
            *err = errf("VA-API frames", r);
            av_buffer_unref(&frames);
            avcodec_free_context(&ctx);
            return -1;
        }
        ctx->hw_frames_ctx = frames;
        ctx->pix_fmt = AV_PIX_FMT_VAAPI;
        av_opt_set(ctx->priv_data, "rc_mode", "CBR", 0);
        av_opt_set_int(ctx->priv_data, "async_depth", 1, 0);
    } else if (e->backend == QV_ENC_X264) {
        ctx->pix_fmt = AV_PIX_FMT_YUV420P;
        av_opt_set(ctx->priv_data, "preset", "veryfast", 0);
        av_opt_set(ctx->priv_data, "tune", screen ? "stillimage,zerolatency" : "zerolatency", 0);
        av_opt_set(ctx->priv_data, "x264-params", "repeat-headers=1:scenecut=0", 0);
    }
    int r = avcodec_open2(ctx, c, NULL);
    if (r < 0) {
        *err = errf("opening the encoder failed", r);
        avcodec_free_context(&ctx);
        return -1;
    }
    e->ctx = ctx;
    e->pkt = av_packet_alloc();
    return 0;
}

static int openOpenH264(qv_lenc *e, int fps, int bitrate, int gop, int screen, char **err) {
    if (WelsCreateSVCEncoder(&e->oh) != 0 || !e->oh) {
        *err = strdup("openh264: cannot create encoder");
        return -1;
    }
    SEncParamExt p;
    (*e->oh)->GetDefaultParams(e->oh, &p);
    p.iUsageType = screen ? SCREEN_CONTENT_REAL_TIME : CAMERA_VIDEO_REAL_TIME;
    p.iPicWidth = e->w;
    p.iPicHeight = e->h;
    p.iTargetBitrate = bitrate;
    p.iMaxBitrate = bitrate * 3 / 2;
    p.iRCMode = RC_BITRATE_MODE;
    p.fMaxFrameRate = (float)fps;
    p.uiIntraPeriod = (unsigned)gop;
    p.bEnableFrameSkip = false;
    p.iMultipleThreadIdc = 1;
    p.eSpsPpsIdStrategy = CONSTANT_ID;
    p.iSpatialLayerNum = 1;
    p.sSpatialLayers[0].iVideoWidth = e->w;
    p.sSpatialLayers[0].iVideoHeight = e->h;
    p.sSpatialLayers[0].fFrameRate = (float)fps;
    p.sSpatialLayers[0].iSpatialBitrate = bitrate;
    p.sSpatialLayers[0].iMaxSpatialBitrate = bitrate * 3 / 2;
    p.sSpatialLayers[0].sSliceArgument.uiSliceMode = SM_SINGLE_SLICE;
    if ((*e->oh)->InitializeExt(e->oh, &p) != 0) {
        *err = strdup("openh264: InitializeExt failed");
        WelsDestroySVCEncoder(e->oh);
        e->oh = NULL;
        return -1;
    }
    int fmt = videoFormatI420;
    (*e->oh)->SetOption(e->oh, ENCODER_OPTION_DATAFORMAT, &fmt);
    return 0;
}

static const char *backendName(int b) {
    switch (b) {
    case QV_ENC_NVENC:
        return "NVENC (h264_nvenc)";
    case QV_ENC_VAAPI:
        return "VA-API (h264_vaapi)";
    case QV_ENC_OPENH264:
        return "openh264 (software)";
    case QV_ENC_X264:
        return "libx264 (software, GPL)";
    }
    return "?";
}

static int tryOpen(qv_lenc *e, int b, int fps, int bitrate, int gop, int screen, char **err) {
    e->backend = b;
    if (b == QV_ENC_OPENH264) return openOpenH264(e, fps, bitrate, gop, screen, err);
    const char *name = b == QV_ENC_NVENC ? "h264_nvenc" : b == QV_ENC_VAAPI ? "h264_vaapi" : "libx264";
    return openAV(e, name, fps, bitrate, gop, screen, err);
}

qv_lenc *qv_lenc_new(int w, int h, int fps, int bitrate, int gop, int screen, int backend, int allow_gpl,
                     uintptr_t handle, char **err, char *name, int name_len) {
    qv_lenc *e = calloc(1, sizeof *e);
    e->w = w & ~1;
    e->h = h & ~1;
    e->handle = handle;
    int order[4], n = 0;
    if (backend != QV_ENC_AUTO) {
        order[n++] = backend;
    } else {
        // Hardware first; openh264 (BSD) as the software fallback; x264 is
        // GPL and only used when explicitly allowed.
        order[n++] = QV_ENC_NVENC;
        order[n++] = QV_ENC_VAAPI;
        order[n++] = QV_ENC_OPENH264;
        if (allow_gpl) order[n++] = QV_ENC_X264;
    }
    char *last = NULL;
    for (int i = 0; i < n; i++) {
        char *e2 = NULL;
        if (tryOpen(e, order[i], fps, bitrate, gop, screen, &e2) == 0) {
            free(last);
            snprintf(name, name_len, "%s", backendName(order[i]));
            return e;
        }
        free(last);
        char buf[512];
        snprintf(buf, sizeof buf, "%s: %s", backendName(order[i]), e2 ? e2 : "failed");
        free(e2);
        last = strdup(buf);
        if (e->hwdev) av_buffer_unref(&e->hwdev);
    }
    *err = last ? last : strdup("no H.264 encoder");
    free(e);
    return NULL;
}

static int drainAV(qv_lenc *e) {
    for (;;) {
        int r = avcodec_receive_packet(e->ctx, e->pkt);
        if (r == AVERROR(EAGAIN) || r == AVERROR_EOF) return 0;
        if (r < 0) return r;
        qvGoLencOutput(e->handle, e->pkt->data, e->pkt->size, (e->pkt->flags & AV_PKT_FLAG_KEY) != 0, e->pkt->pts);
        av_packet_unref(e->pkt);
    }
}

int qv_lenc_encode(qv_lenc *e, AVFrame *src, int64_t us, int force) {
    if (e->oh) {
        if (ensureConv(e, src, AV_PIX_FMT_YUV420P) < 0) return -1;
        if (force) (*e->oh)->ForceIntraFrame(e->oh, true);
        SSourcePicture pic = {0};
        pic.iColorFormat = videoFormatI420;
        pic.iPicWidth = e->w;
        pic.iPicHeight = e->h;
        for (int i = 0; i < 3; i++) {
            pic.iStride[i] = e->conv->linesize[i];
            pic.pData[i] = e->conv->data[i];
        }
        pic.uiTimeStamp = us / 1000; // ms
        SFrameBSInfo info;
        memset(&info, 0, sizeof info);
        if ((*e->oh)->EncodeFrame(e->oh, &pic, &info) != cmResultSuccess) return -1;
        if (info.eFrameType == videoFrameTypeSkip || info.iFrameSizeInBytes == 0) return 0;
        // Layers are contiguous in memory: the first layer's buffer holds them all.
        uint8_t *start = info.sLayerInfo[0].pBsBuf;
        qvGoLencOutput(e->handle, start, info.iFrameSizeInBytes, info.eFrameType == videoFrameTypeIDR, us);
        return 0;
    }
    AVFrame *in;
    AVFrame *hw = NULL;
    enum AVPixelFormat sw = e->backend == QV_ENC_X264 ? AV_PIX_FMT_YUV420P : AV_PIX_FMT_NV12;
    if (ensureConv(e, src, sw) < 0) return -1;
    in = e->conv;
    if (e->backend == QV_ENC_VAAPI) {
        hw = av_frame_alloc();
        if (av_hwframe_get_buffer(e->ctx->hw_frames_ctx, hw, 0) < 0 || av_hwframe_transfer_data(hw, e->conv, 0) < 0) {
            av_frame_free(&hw);
            return -1;
        }
        in = hw;
    }
    in->pts = us;
    in->pict_type = force ? AV_PICTURE_TYPE_I : AV_PICTURE_TYPE_NONE;
    if (force) in->flags |= AV_FRAME_FLAG_KEY;
    else in->flags &= ~AV_FRAME_FLAG_KEY;
    int r = avcodec_send_frame(e->ctx, in);
    av_frame_free(&hw);
    if (r < 0) return r;
    return drainAV(e);
}

void qv_lenc_close(qv_lenc *e) {
    if (!e) return;
    if (e->ctx) {
        avcodec_send_frame(e->ctx, NULL); // flush
        drainAV(e);
        avcodec_free_context(&e->ctx);
    }
    if (e->oh) {
        (*e->oh)->Uninitialize(e->oh);
        WelsDestroySVCEncoder(e->oh);
    }
    av_packet_free(&e->pkt);
    av_frame_free(&e->conv);
    sws_freeContext(e->sws);
    av_buffer_unref(&e->hwdev);
    free(e);
}

// ---------------------------------------------------------------- decoder

struct qv_ldec {
    AVCodecContext *ctx;
    AVPacket *pkt;
    AVFrame *frame;
    uintptr_t handle;
};

qv_ldec *qv_ldec_new(uintptr_t handle, char **err) {
    const AVCodec *c = avcodec_find_decoder(AV_CODEC_ID_H264);
    if (!c) {
        *err = strdup("no H.264 decoder in libavcodec");
        return NULL;
    }
    qv_ldec *d = calloc(1, sizeof *d);
    d->handle = handle;
    d->ctx = avcodec_alloc_context3(c);
    d->ctx->flags |= AV_CODEC_FLAG_LOW_DELAY;
    d->ctx->thread_count = 2;
    d->ctx->thread_type = FF_THREAD_SLICE; // frame threads add a frame of delay
    int r = avcodec_open2(d->ctx, c, NULL);
    if (r < 0) {
        *err = errf("opening the decoder failed", r);
        avcodec_free_context(&d->ctx);
        free(d);
        return NULL;
    }
    d->pkt = av_packet_alloc();
    d->frame = av_frame_alloc();
    return d;
}

int qv_ldec_decode(qv_ldec *d, const uint8_t *data, int len, int64_t us) {
    if (av_new_packet(d->pkt, len) < 0) return -1;
    memcpy(d->pkt->data, data, len);
    d->pkt->pts = us;
    int r = avcodec_send_packet(d->ctx, d->pkt);
    av_packet_unref(d->pkt);
    if (r < 0) return r;
    for (;;) {
        r = avcodec_receive_frame(d->ctx, d->frame);
        if (r == AVERROR(EAGAIN) || r == AVERROR_EOF) return 0;
        if (r < 0) return r;
        AVFrame *out = av_frame_clone(d->frame); // Go owns this reference
        av_frame_unref(d->frame);
        qvGoLdecOutput(d->handle, out, out->pts);
    }
}

void qv_ldec_close(qv_ldec *d) {
    if (!d) return;
    avcodec_free_context(&d->ctx);
    av_packet_free(&d->pkt);
    av_frame_free(&d->frame);
    free(d);
}
