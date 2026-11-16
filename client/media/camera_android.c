//go:build android && cgo

// Android camera capture (Camera2 NDK: ACameraManager + AImageReader) and
// H.264 encoding (AMediaCodec, hardware). Decoding isn't here: the boson
// app decodes in its WebView (WebCodecs).
#include <camera/NdkCameraDevice.h>
#include <camera/NdkCameraManager.h>
#include <camera/NdkCameraMetadata.h>
#include <media/NdkImage.h>
#include <media/NdkImageReader.h>
#include <media/NdkMediaCodec.h>
#include <media/NdkMediaFormat.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include "media_android.h"

extern void qvGoACamFrame(uintptr_t handle, const uint8_t *y, int ystride, const uint8_t *u, const uint8_t *v,
                          int uvstride, int uvpixel, int width, int height, int64_t us);
extern void qvGoACamStopped(uintptr_t handle, char *msg);
extern void qvGoAEncOut(uintptr_t handle, const uint8_t *data, size_t size, int keyframe, int64_t us);

static int64_t nowUS(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (int64_t)ts.tv_sec * 1000000 + ts.tv_nsec / 1000;
}

static char *errs(const char *what, int code) {
    char buf[160];
    snprintf(buf, sizeof buf, "%s (%d)", what, code);
    return strdup(buf);
}

// ---------------------------------------------------------------- list

static void appendf(char **buf, size_t *len, size_t *cap, const char *fmt, ...) __attribute__((format(printf, 4, 5)));
#include <stdarg.h>
static void appendf(char **buf, size_t *len, size_t *cap, const char *fmt, ...) {
    va_list ap;
    for (;;) {
        va_start(ap, fmt);
        int n = vsnprintf(*buf + *len, *cap - *len, fmt, ap);
        va_end(ap);
        if (n >= 0 && (size_t)n < *cap - *len) {
            *len += n;
            return;
        }
        *cap *= 2;
        *buf = realloc(*buf, *cap);
    }
}

char *qv_acam_list(char **err) {
    ACameraManager *m = ACameraManager_create();
    ACameraIdList *ids = NULL;
    camera_status_t st = ACameraManager_getCameraIdList(m, &ids);
    if (st != ACAMERA_OK) {
        ACameraManager_delete(m);
        *err = errs("listing cameras failed", st);
        return NULL;
    }
    size_t cap = 1024, len = 0;
    char *out = malloc(cap);
    out[0] = 0;
    appendf(&out, &len, &cap, "[");
    for (int i = 0; i < ids->numCameras; i++) {
        const char *id = ids->cameraIds[i];
        ACameraMetadata *md = NULL;
        if (ACameraManager_getCameraCharacteristics(m, id, &md) != ACAMERA_OK) continue;
        ACameraMetadata_const_entry e;
        int facing = 2, orient = 0;
        if (ACameraMetadata_getConstEntry(md, ACAMERA_LENS_FACING, &e) == ACAMERA_OK) facing = e.data.u8[0];
        if (ACameraMetadata_getConstEntry(md, ACAMERA_SENSOR_ORIENTATION, &e) == ACAMERA_OK) orient = e.data.i32[0];
        int maxfps = 30;
        if (ACameraMetadata_getConstEntry(md, ACAMERA_CONTROL_AE_AVAILABLE_TARGET_FPS_RANGES, &e) == ACAMERA_OK) {
            maxfps = 0;
            for (uint32_t k = 0; k + 1 < e.count; k += 2)
                if (e.data.i32[k + 1] > maxfps) maxfps = e.data.i32[k + 1];
        }
        const char *name = facing == ACAMERA_LENS_FACING_FRONT ? "Front camera"
                           : facing == ACAMERA_LENS_FACING_BACK ? "Back camera"
                                                                : "External camera";
        appendf(&out, &len, &cap, "%s{\"id\":\"%s\",\"name\":\"%s\",\"kind\":\"%s\",\"facing\":%d,\"orientation\":%d,\"formats\":[",
                len > 1 ? "," : "", id, name, facing == ACAMERA_LENS_FACING_EXTERNAL ? "external" : "builtin", facing, orient);
        int first = 1;
        if (ACameraMetadata_getConstEntry(md, ACAMERA_SCALER_AVAILABLE_STREAM_CONFIGURATIONS, &e) == ACAMERA_OK) {
            for (uint32_t k = 0; k + 3 < e.count; k += 4) {
                // format, width, height, input?
                if (e.data.i32[k] != AIMAGE_FORMAT_YUV_420_888 || e.data.i32[k + 3]) continue;
                appendf(&out, &len, &cap, "%s{\"width\":%d,\"height\":%d,\"max_fps\":%d}", first ? "" : ",",
                        e.data.i32[k + 1], e.data.i32[k + 2], maxfps);
                first = 0;
            }
        }
        appendf(&out, &len, &cap, "]}");
        ACameraMetadata_free(md);
    }
    appendf(&out, &len, &cap, "]");
    ACameraManager_deleteCameraIdList(ids);
    ACameraManager_delete(m);
    return out;
}

// ---------------------------------------------------------------- capture

struct qv_acam {
    ACameraManager *mgr;
    ACameraDevice *dev;
    AImageReader *reader;
    ANativeWindow *window;
    ACaptureSessionOutputContainer *outputs;
    ACaptureSessionOutput *output;
    ACameraOutputTarget *target;
    ACaptureRequest *request;
    ACameraCaptureSession *session;
    uintptr_t handle;
    volatile int stopping;
};

static void onImage(void *ctx, AImageReader *r) {
    struct qv_acam *c = ctx;
    AImage *img = NULL;
    if (AImageReader_acquireLatestImage(r, &img) != AMEDIA_OK || !img) return;
    if (!c->stopping) {
        int32_t w = 0, h = 0, ys = 0, uvs = 0, uvp = 0;
        uint8_t *y = NULL, *u = NULL, *v = NULL;
        int yl = 0, ul = 0, vl = 0;
        AImage_getWidth(img, &w);
        AImage_getHeight(img, &h);
        AImage_getPlaneData(img, 0, &y, &yl);
        AImage_getPlaneData(img, 1, &u, &ul);
        AImage_getPlaneData(img, 2, &v, &vl);
        AImage_getPlaneRowStride(img, 0, &ys);
        AImage_getPlaneRowStride(img, 1, &uvs);
        AImage_getPlanePixelStride(img, 1, &uvp);
        if (y && u && v) qvGoACamFrame(c->handle, y, ys, u, v, uvs, uvp, w, h, nowUS());
    }
    AImage_delete(img);
}

static void onDisconnected(void *ctx, ACameraDevice *d) {
    struct qv_acam *c = ctx;
    if (!c->stopping) qvGoACamStopped(c->handle, strdup("the camera was disconnected (or taken by another app)"));
}

static void onError(void *ctx, ACameraDevice *d, int e) {
    struct qv_acam *c = ctx;
    if (!c->stopping) qvGoACamStopped(c->handle, errs("camera error", e));
}

static void sessClosed(void *ctx, ACameraCaptureSession *s) {}
static void sessReady(void *ctx, ACameraCaptureSession *s) {}
static void sessActive(void *ctx, ACameraCaptureSession *s) {}

qv_acam *qv_acam_start(const char *id, int width, int height, int fps, uintptr_t handle, char **err) {
    struct qv_acam *c = calloc(1, sizeof *c);
    c->handle = handle;
    c->mgr = ACameraManager_create();
    media_status_t ms = AImageReader_new(width, height, AIMAGE_FORMAT_YUV_420_888, 4, &c->reader);
    if (ms != AMEDIA_OK) {
        *err = errs("image reader", ms);
        qv_acam_stop(c);
        return NULL;
    }
    AImageReader_ImageListener l = {.context = c, .onImageAvailable = onImage};
    AImageReader_setImageListener(c->reader, &l);
    AImageReader_getWindow(c->reader, &c->window);
    ACameraDevice_StateCallbacks dcb = {.context = c, .onDisconnected = onDisconnected, .onError = onError};
    camera_status_t st = ACameraManager_openCamera(c->mgr, id, &dcb, &c->dev);
    if (st != ACAMERA_OK) {
        *err = st == ACAMERA_ERROR_PERMISSION_DENIED ? strdup("camera permission denied") : errs("opening the camera failed", st);
        qv_acam_stop(c);
        return NULL;
    }
    ACaptureSessionOutputContainer_create(&c->outputs);
    ACaptureSessionOutput_create(c->window, &c->output);
    ACaptureSessionOutputContainer_add(c->outputs, c->output);
    ACameraOutputTarget_create(c->window, &c->target);
    ACameraDevice_createCaptureRequest(c->dev, TEMPLATE_RECORD, &c->request);
    ACaptureRequest_addTarget(c->request, c->target);
    int32_t range[2] = {fps, fps};
    ACaptureRequest_setEntry_i32(c->request, ACAMERA_CONTROL_AE_TARGET_FPS_RANGE, 2, range);
    ACameraCaptureSession_stateCallbacks scb = {.context = c, .onClosed = sessClosed, .onReady = sessReady, .onActive = sessActive};
    st = ACameraDevice_createCaptureSession(c->dev, c->outputs, &scb, &c->session);
    if (st == ACAMERA_OK) st = ACameraCaptureSession_setRepeatingRequest(c->session, NULL, 1, &c->request, NULL);
    if (st != ACAMERA_OK) {
        *err = errs("starting the camera failed", st);
        qv_acam_stop(c);
        return NULL;
    }
    return c;
}

// qv_acam_stop releases everything; no frame reaches Go after it returns
// (the reader is deleted last, which waits for its listener).
void qv_acam_stop(qv_acam *c) {
    if (!c) return;
    c->stopping = 1;
    if (c->session) {
        ACameraCaptureSession_stopRepeating(c->session);
        ACameraCaptureSession_close(c->session);
    }
    if (c->request) ACaptureRequest_free(c->request);
    if (c->target) ACameraOutputTarget_free(c->target);
    if (c->outputs) {
        if (c->output) ACaptureSessionOutputContainer_remove(c->outputs, c->output);
        ACaptureSessionOutputContainer_free(c->outputs);
    }
    if (c->output) ACaptureSessionOutput_free(c->output);
    if (c->dev) ACameraDevice_close(c->dev);
    if (c->reader) AImageReader_delete(c->reader);
    if (c->mgr) ACameraManager_delete(c->mgr);
    free(c);
}

// ---------------------------------------------------------------- encoder

struct qv_aenc {
    AMediaCodec *codec;
    int width, height;
    uintptr_t handle;
    uint8_t *config; // SPS/PPS (codec config), put before each keyframe
    size_t config_len;
    int planar; // the encoder takes I420 (else NV12)
};

#define COLOR_FormatYUV420SemiPlanar 21
#define BUFFER_FLAG_KEY_FRAME 1
#define BUFFER_FLAG_CODEC_CONFIG 2

// Encoders differ in the keys and input layouts they accept (the
// emulator's software c2.android.avc.encoder refuses some hints): try
// the full setup, then without the optional hints, then minimal, each
// with NV12 and then I420 input.
static AMediaCodec *configure(int width, int height, int fps, int bitrate, int gop, int level, int color) {
    AMediaCodec *codec = AMediaCodec_createEncoderByType("video/avc");
    if (!codec) return NULL;
    AMediaFormat *f = AMediaFormat_new();
    AMediaFormat_setString(f, AMEDIAFORMAT_KEY_MIME, "video/avc");
    AMediaFormat_setInt32(f, AMEDIAFORMAT_KEY_WIDTH, width);
    AMediaFormat_setInt32(f, AMEDIAFORMAT_KEY_HEIGHT, height);
    AMediaFormat_setInt32(f, AMEDIAFORMAT_KEY_BIT_RATE, bitrate);
    AMediaFormat_setInt32(f, AMEDIAFORMAT_KEY_FRAME_RATE, fps);
    AMediaFormat_setInt32(f, AMEDIAFORMAT_KEY_I_FRAME_INTERVAL, gop > 0 ? gop : 2);
    AMediaFormat_setInt32(f, AMEDIAFORMAT_KEY_COLOR_FORMAT, color);
    if (level >= 1) AMediaFormat_setInt32(f, "bitrate-mode", 2); // CBR
    if (level >= 2) {
        AMediaFormat_setInt32(f, "profile", 1); // Baseline: no B-frames
        AMediaFormat_setInt32(f, "level", 0x200); // AVCLevel31
        AMediaFormat_setInt32(f, "latency", 1);
        AMediaFormat_setInt32(f, "prepend-sps-pps-to-idr-frames", 1);
    }
    media_status_t st = AMediaCodec_configure(codec, f, NULL, NULL, AMEDIACODEC_CONFIGURE_FLAG_ENCODE);
    AMediaFormat_delete(f);
    if (st == AMEDIA_OK) st = AMediaCodec_start(codec);
    if (st != AMEDIA_OK) {
        AMediaCodec_delete(codec);
        return NULL;
    }
    return codec;
}

#define COLOR_FormatYUV420Planar 19

qv_aenc *qv_aenc_new(int width, int height, int fps, int bitrate, int gop_seconds, uintptr_t handle, char **err) {
    static const int colors[] = {COLOR_FormatYUV420SemiPlanar, COLOR_FormatYUV420Planar};
    AMediaCodec *codec = NULL;
    int color = 0;
    for (int c = 0; c < 2 && !codec; c++) {
        for (int level = 2; level >= 0 && !codec; level--) {
            codec = configure(width, height, fps, bitrate, gop_seconds, level, colors[c]);
            if (codec) color = colors[c];
        }
    }
    if (!codec) {
        *err = strdup("no H.264 encoder accepted the format");
        return NULL;
    }
    struct qv_aenc *e = calloc(1, sizeof *e);
    e->codec = codec;
    e->width = width;
    e->height = height;
    e->handle = handle;
    e->planar = color == COLOR_FormatYUV420Planar;
    return e;
}

static void drain(struct qv_aenc *e) {
    for (;;) {
        AMediaCodecBufferInfo info;
        ssize_t idx = AMediaCodec_dequeueOutputBuffer(e->codec, &info, 0);
        if (idx < 0) return; // try again later / format changed
        size_t cap = 0;
        uint8_t *buf = AMediaCodec_getOutputBuffer(e->codec, idx, &cap);
        if (buf && info.size > 0) {
            const uint8_t *data = buf + info.offset;
            if (info.flags & BUFFER_FLAG_CODEC_CONFIG) {
                free(e->config);
                e->config = malloc(info.size);
                memcpy(e->config, data, info.size);
                e->config_len = info.size;
            } else {
                int key = (info.flags & BUFFER_FLAG_KEY_FRAME) != 0;
                if (key && e->config_len) {
                    uint8_t *au = malloc(e->config_len + info.size);
                    memcpy(au, e->config, e->config_len);
                    memcpy(au + e->config_len, data, info.size);
                    qvGoAEncOut(e->handle, au, e->config_len + info.size, 1, info.presentationTimeUs);
                    free(au);
                } else {
                    qvGoAEncOut(e->handle, data, info.size, key, info.presentationTimeUs);
                }
            }
        }
        AMediaCodec_releaseOutputBuffer(e->codec, idx, 0);
    }
}

int qv_aenc_encode(qv_aenc *e, const uint8_t *nv12, int64_t us, int force) {
    if (force) {
        AMediaFormat *p = AMediaFormat_new();
        AMediaFormat_setInt32(p, "request-sync", 0);
        AMediaCodec_setParameters(e->codec, p);
        AMediaFormat_delete(p);
    }
    ssize_t idx = AMediaCodec_dequeueInputBuffer(e->codec, 20000);
    if (idx < 0) {
        drain(e);
        return -1; // encoder full: frame dropped
    }
    size_t cap = 0;
    uint8_t *buf = AMediaCodec_getInputBuffer(e->codec, idx, &cap);
    size_t size = (size_t)e->width * e->height * 3 / 2;
    if (!buf || cap < size) {
        AMediaCodec_queueInputBuffer(e->codec, idx, 0, 0, us, 0);
        return -2;
    }
    size_t ysz = (size_t)e->width * e->height;
    memcpy(buf, nv12, ysz);
    if (e->planar) { // NV12 → I420: de-interleave the chroma
        uint8_t *u = buf + ysz, *v = u + ysz / 4;
        const uint8_t *uv = nv12 + ysz;
        for (size_t i = 0; i < ysz / 4; i++) {
            u[i] = uv[2 * i];
            v[i] = uv[2 * i + 1];
        }
    } else {
        memcpy(buf + ysz, nv12 + ysz, ysz / 2);
    }
    AMediaCodec_queueInputBuffer(e->codec, idx, 0, size, us, 0);
    drain(e);
    return 0;
}

void qv_aenc_close(qv_aenc *e) {
    if (!e) return;
    AMediaCodec_stop(e->codec);
    AMediaCodec_delete(e->codec);
    free(e->config);
    free(e);
}
