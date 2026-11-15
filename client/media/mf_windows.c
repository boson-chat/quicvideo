//go:build windows && cgo

// Media Foundation: H.264 encoder MFTs (hardware preferred, asynchronous;
// the Microsoft software encoder as fallback, synchronous), the H.264
// decoder MFT, and webcams through the source reader.
#define COBJMACROS
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <codecapi.h>
#include <mfapi.h>
#include <mferror.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <mftransform.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <wmcodecdsp.h>
#include "media_windows.h"

// codecapi.h only defines CODECAPI_* GUIDs for C++ (__uuidof); build them
// from its STATIC_CODECAPI_* values.
#define QV_CODECAPI(n) static const GUID QV_##n = {STATIC_CODECAPI_##n};
QV_CODECAPI(AVEncCommonRateControlMode)
QV_CODECAPI(AVEncCommonMeanBitRate)
QV_CODECAPI(AVEncMPVGOPSize)
QV_CODECAPI(AVEncMPVDefaultBPictureCount)
QV_CODECAPI(AVLowLatencyMode)
QV_CODECAPI(AVEncVideoForceKeyFrame)

// mfapi.h's MFSetAttributeSize & co. are C++ inline helpers.
static HRESULT setPair(IMFAttributes *a, REFGUID k, UINT32 hi, UINT32 lo) {
    return IMFAttributes_SetUINT64(a, k, ((UINT64)hi << 32) | lo);
}
static HRESULT getPair(IMFAttributes *a, REFGUID k, UINT32 *hi, UINT32 *lo) {
    UINT64 v = 0;
    HRESULT hr = IMFAttributes_GetUINT64(a, k, &v);
    *hi = (UINT32)(v >> 32);
    *lo = (UINT32)v;
    return hr;
}
#define MFSetAttributeSize setPair
#define MFSetAttributeRatio setPair
#define MFGetAttributeSize getPair
#define MFGetAttributeRatio getPair

extern void qvGoWencOutput(uintptr_t handle, uint8_t *data, int len, int key, int64_t us);
extern void qvGoWdecOutput(uintptr_t handle, qv_wpic *p, int64_t us);
extern void qvGoWcapFrame(uintptr_t handle, qv_wpic *p, int64_t us);

static char *hrerr(const char *what, HRESULT hr) {
    char buf[256];
    snprintf(buf, sizeof buf, "%s (HRESULT 0x%08lx)", what, (unsigned long)hr);
    return strdup(buf);
}

static void coinit(void) { CoInitializeEx(NULL, COINIT_MULTITHREADED); } // S_FALSE / mode-changed are fine

static int64_t nowUS(void) {
    FILETIME ft;
    GetSystemTimePreciseAsFileTime(&ft);
    uint64_t t = ((uint64_t)ft.dwHighDateTime << 32) | ft.dwLowDateTime; // 100 ns since 1601
    return (int64_t)(t / 10) - 11644473600000000LL;
}

int qv_mf_startup(char **err) {
    coinit();
    HRESULT hr = MFStartup(MF_VERSION, MFSTARTUP_FULL);
    if (FAILED(hr)) {
        *err = hrerr("Media Foundation is not available (on Windows Server, install the Media Foundation feature)", hr);
        return -1;
    }
    return 0;
}

// ---------------------------------------------------------------- encoder

struct qv_wenc {
    IMFTransform *mft;
    ICodecAPI *codec;
    IMFMediaEventGenerator *events; // non-NULL for async (hardware) MFTs
    int w, h, fps;
    uintptr_t handle;
    int needInput; // async: pending METransformNeedInput events
    BYTE *seqHeader;
    UINT32 seqLen;
};

static void setUI4(ICodecAPI *c, const GUID *api, ULONG v) {
    VARIANT var;
    VariantInit(&var);
    var.vt = VT_UI4;
    var.ulVal = v;
    ICodecAPI_SetValue(c, api, &var);
}

static void setBool(ICodecAPI *c, const GUID *api, BOOL v) {
    VARIANT var;
    VariantInit(&var);
    var.vt = VT_BOOL;
    var.boolVal = v ? VARIANT_TRUE : VARIANT_FALSE;
    ICodecAPI_SetValue(c, api, &var);
}

static HRESULT configure(struct qv_wenc *e, int bitrate, int gop) {
    IMFMediaType *out = NULL, *in = NULL;
    HRESULT hr = MFCreateMediaType(&out);
    if (FAILED(hr)) return hr;
    IMFMediaType_SetGUID(out, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
    IMFMediaType_SetGUID(out, &MF_MT_SUBTYPE, &MFVideoFormat_H264);
    IMFMediaType_SetUINT32(out, &MF_MT_AVG_BITRATE, bitrate);
    MFSetAttributeSize((IMFAttributes *)out, &MF_MT_FRAME_SIZE, e->w, e->h);
    MFSetAttributeRatio((IMFAttributes *)out, &MF_MT_FRAME_RATE, e->fps, 1);
    MFSetAttributeRatio((IMFAttributes *)out, &MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
    IMFMediaType_SetUINT32(out, &MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
    IMFMediaType_SetUINT32(out, &MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_Main);
    hr = IMFTransform_SetOutputType(e->mft, 0, out, 0);
    IMFMediaType_Release(out);
    if (FAILED(hr)) return hr;

    if (FAILED(hr = MFCreateMediaType(&in))) return hr;
    IMFMediaType_SetGUID(in, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
    IMFMediaType_SetGUID(in, &MF_MT_SUBTYPE, &MFVideoFormat_NV12);
    MFSetAttributeSize((IMFAttributes *)in, &MF_MT_FRAME_SIZE, e->w, e->h);
    MFSetAttributeRatio((IMFAttributes *)in, &MF_MT_FRAME_RATE, e->fps, 1);
    MFSetAttributeRatio((IMFAttributes *)in, &MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
    IMFMediaType_SetUINT32(in, &MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
    hr = IMFTransform_SetInputType(e->mft, 0, in, 0);
    IMFMediaType_Release(in);
    if (FAILED(hr)) return hr;

    if (SUCCEEDED(IMFTransform_QueryInterface(e->mft, &IID_ICodecAPI, (void **)&e->codec))) {
        setUI4(e->codec, &QV_AVEncCommonRateControlMode, eAVEncCommonRateControlMode_CBR);
        setUI4(e->codec, &QV_AVEncCommonMeanBitRate, bitrate);
        setUI4(e->codec, &QV_AVEncMPVGOPSize, gop);
        setUI4(e->codec, &QV_AVEncMPVDefaultBPictureCount, 0);
        setBool(e->codec, &QV_AVLowLatencyMode, TRUE);
    }
    // Keep the sequence header (SPS/PPS) to prepend if an encoder doesn't
    // repeat it in-band on keyframes.
    IMFMediaType *cur = NULL;
    if (SUCCEEDED(IMFTransform_GetOutputCurrentType(e->mft, 0, &cur))) {
        IMFMediaType_GetAllocatedBlob(cur, &MF_MT_MPEG_SEQUENCE_HEADER, &e->seqHeader, &e->seqLen);
        IMFMediaType_Release(cur);
    }
    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
    return S_OK;
}

static int findSPS(const BYTE *p, DWORD n) {
    for (DWORD i = 0; i + 4 < n; i++)
        if (p[i] == 0 && p[i + 1] == 0 && p[i + 2] == 1 && (p[i + 3] & 0x1f) == 7) return 1;
    return 0;
}

// emit sends one output sample to Go.
static void emit(struct qv_wenc *e, IMFSample *s) {
    IMFMediaBuffer *buf = NULL;
    if (FAILED(IMFSample_ConvertToContiguousBuffer(s, &buf))) return;
    BYTE *data;
    DWORD len;
    if (SUCCEEDED(IMFMediaBuffer_Lock(buf, &data, NULL, &len))) {
        UINT32 clean = 0;
        IMFSample_GetUINT32(s, &MFSampleExtension_CleanPoint, &clean);
        LONGLONG t = 0;
        IMFSample_GetSampleTime(s, &t);
        if (clean && e->seqHeader && !findSPS(data, len)) {
            BYTE *all = malloc(e->seqLen + len);
            memcpy(all, e->seqHeader, e->seqLen);
            memcpy(all + e->seqLen, data, len);
            qvGoWencOutput(e->handle, all, (int)(e->seqLen + len), 1, t / 10);
            free(all);
        } else {
            qvGoWencOutput(e->handle, data, (int)len, clean != 0, t / 10);
        }
        IMFMediaBuffer_Unlock(buf);
    }
    IMFMediaBuffer_Release(buf);
}

// pull drains available output (sync MFTs, or one HaveOutput event).
static HRESULT pull(struct qv_wenc *e) {
    for (;;) {
        MFT_OUTPUT_STREAM_INFO info = {0};
        IMFTransform_GetOutputStreamInfo(e->mft, 0, &info);
        MFT_OUTPUT_DATA_BUFFER out = {0};
        IMFSample *own = NULL;
        if (!(info.dwFlags & (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES | MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES))) {
            IMFMediaBuffer *b = NULL;
            MFCreateSample(&own);
            MFCreateMemoryBuffer(info.cbSize ? info.cbSize : (DWORD)(e->w * e->h * 2), &b);
            IMFSample_AddBuffer(own, b);
            IMFMediaBuffer_Release(b);
            out.pSample = own;
        }
        DWORD status = 0;
        HRESULT hr = IMFTransform_ProcessOutput(e->mft, 0, 1, &out, &status);
        if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) {
            if (own) IMFSample_Release(own);
            return S_OK;
        }
        if (hr == MF_E_TRANSFORM_STREAM_CHANGE) { // re-negotiate the output type
            IMFMediaType *t = NULL;
            if (own) IMFSample_Release(own);
            if (SUCCEEDED(IMFTransform_GetOutputAvailableType(e->mft, 0, 0, &t))) {
                IMFTransform_SetOutputType(e->mft, 0, t, 0);
                IMFMediaType_Release(t);
            }
            continue;
        }
        if (FAILED(hr)) {
            if (own) IMFSample_Release(own);
            return hr;
        }
        if (out.pSample) {
            emit(e, out.pSample);
            if (out.pSample != own) IMFSample_Release(out.pSample);
        }
        if (own) IMFSample_Release(own);
        if (out.pEvents) IMFCollection_Release(out.pEvents);
        if (e->events) return S_OK; // async: one output per HaveOutput event
    }
}

// pumpEvents handles queued async events; with wait, blocks until the MFT
// wants input (or timeout).
static HRESULT pumpEvents(struct qv_wenc *e, int wait) {
    DWORD deadline = GetTickCount() + 2000;
    for (;;) {
        IMFMediaEvent *ev = NULL;
        HRESULT hr = IMFMediaEventGenerator_GetEvent(e->events, MF_EVENT_FLAG_NO_WAIT, &ev);
        if (hr == MF_E_NO_EVENTS_AVAILABLE) {
            if (!wait || e->needInput > 0) return S_OK;
            if (GetTickCount() > deadline) return MF_E_NO_EVENTS_AVAILABLE;
            Sleep(1);
            continue;
        }
        if (FAILED(hr)) return hr;
        MediaEventType type;
        IMFMediaEvent_GetType(ev, &type);
        IMFMediaEvent_Release(ev);
        if (type == METransformNeedInput) e->needInput++;
        else if (type == METransformHaveOutput) pull(e);
    }
}

static HRESULT activate(IMFActivate *a, struct qv_wenc *e, char *name, int name_len) {
    HRESULT hr = IMFActivate_ActivateObject(a, &IID_IMFTransform, (void **)&e->mft);
    if (FAILED(hr)) return hr;
    WCHAR *wname = NULL;
    UINT32 n = 0;
    if (SUCCEEDED(IMFActivate_GetAllocatedString(a, &MFT_FRIENDLY_NAME_Attribute, &wname, &n))) {
        WideCharToMultiByte(CP_UTF8, 0, wname, -1, name, name_len, NULL, NULL);
        CoTaskMemFree(wname);
    }
    IMFAttributes *attrs = NULL;
    if (SUCCEEDED(IMFTransform_GetAttributes(e->mft, &attrs))) {
        UINT32 async = 0;
        IMFAttributes_GetUINT32(attrs, &MF_TRANSFORM_ASYNC, &async);
        if (async) {
            IMFAttributes_SetUINT32(attrs, &MF_TRANSFORM_ASYNC_UNLOCK, TRUE);
            IMFTransform_QueryInterface(e->mft, &IID_IMFMediaEventGenerator, (void **)&e->events);
        }
        IMFAttributes_Release(attrs);
    }
    return S_OK;
}

static void releaseEnc(struct qv_wenc *e) {
    if (e->codec) ICodecAPI_Release(e->codec);
    if (e->events) IMFMediaEventGenerator_Release(e->events);
    if (e->mft) IMFTransform_Release(e->mft);
    if (e->seqHeader) CoTaskMemFree(e->seqHeader);
    e->codec = NULL;
    e->events = NULL;
    e->mft = NULL;
    e->seqHeader = NULL;
}

qv_wenc *qv_wenc_new(int w, int h, int fps, int bitrate, int gop, int screen, int prefer_hw, uintptr_t handle,
                     char **err, char *name, int name_len) {
    if (qv_mf_startup(err) < 0) return NULL;
    struct qv_wenc *e = calloc(1, sizeof *e);
    e->w = w & ~1;
    e->h = h & ~1;
    e->fps = fps > 0 ? fps : 30;
    e->handle = handle;
    MFT_REGISTER_TYPE_INFO in = {MFMediaType_Video, MFVideoFormat_NV12};
    MFT_REGISTER_TYPE_INFO out = {MFMediaType_Video, MFVideoFormat_H264};
    UINT32 passes[2] = {MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER,
                        MFT_ENUM_FLAG_SYNCMFT | MFT_ENUM_FLAG_LOCALMFT | MFT_ENUM_FLAG_SORTANDFILTER};
    HRESULT last = E_FAIL;
    for (int p = prefer_hw ? 0 : 1; p < 2; p++) {
        IMFActivate **acts = NULL;
        UINT32 count = 0;
        if (FAILED(MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER, passes[p], &in, &out, &acts, &count))) continue;
        for (UINT32 i = 0; i < count; i++) {
            if (!e->mft && SUCCEEDED(last = activate(acts[i], e, name, name_len))) {
                if (FAILED(last = configure(e, bitrate, gop))) releaseEnc(e);
            }
            IMFActivate_Release(acts[i]);
        }
        CoTaskMemFree(acts);
        if (e->mft) return e;
    }
    *err = hrerr("no usable H.264 encoder MFT", last);
    free(e);
    return NULL;
}

int qv_wenc_encode(qv_wenc *e, qv_wpic *p, int64_t us, int force) {
    coinit();
    if (e->events) {
        HRESULT hr = pumpEvents(e, 1);
        if (FAILED(hr) || e->needInput == 0) return -1;
        e->needInput--;
    }
    if (force && e->codec) setUI4(e->codec, &QV_AVEncVideoForceKeyFrame, 1);
    DWORD ysize = (DWORD)e->w * e->h, size = ysize * 3 / 2;
    IMFMediaBuffer *buf = NULL;
    IMFSample *s = NULL;
    if (FAILED(MFCreateMemoryBuffer(size, &buf))) return -1;
    BYTE *dst;
    IMFMediaBuffer_Lock(buf, &dst, NULL, NULL);
    qv_bgra_to_nv12(p->data, p->w, p->h, p->stride, dst, e->w, dst + ysize, e->w, e->w, e->h);
    IMFMediaBuffer_Unlock(buf);
    IMFMediaBuffer_SetCurrentLength(buf, size);
    MFCreateSample(&s);
    IMFSample_AddBuffer(s, buf);
    IMFMediaBuffer_Release(buf);
    IMFSample_SetSampleTime(s, us * 10);
    IMFSample_SetSampleDuration(s, 10000000LL / e->fps);
    HRESULT hr = IMFTransform_ProcessInput(e->mft, 0, s, 0);
    IMFSample_Release(s);
    if (FAILED(hr)) return -1;
    if (e->events) return SUCCEEDED(pumpEvents(e, 0)) ? 0 : -1;
    return SUCCEEDED(pull(e)) ? 0 : -1;
}

void qv_wenc_close(qv_wenc *e) {
    if (!e) return;
    coinit();
    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_COMMAND_DRAIN, 0);
    if (e->events) pumpEvents(e, 0);
    else pull(e);
    releaseEnc(e);
    free(e);
}

// ---------------------------------------------------------------- decoder

struct qv_wdec {
    IMFTransform *mft;
    uintptr_t handle;
    int w, h, stride, vh; // vh: allocated (padded) height
};

static HRESULT setDecOutput(struct qv_wdec *d) {
    for (DWORD i = 0;; i++) {
        IMFMediaType *t = NULL;
        HRESULT hr = IMFTransform_GetOutputAvailableType(d->mft, 0, i, &t);
        if (FAILED(hr)) return hr;
        GUID sub;
        IMFMediaType_GetGUID(t, &MF_MT_SUBTYPE, &sub);
        if (IsEqualGUID(&sub, &MFVideoFormat_NV12)) {
            hr = IMFTransform_SetOutputType(d->mft, 0, t, 0);
            UINT32 w = 0, h = 0;
            MFGetAttributeSize((IMFAttributes *)t, &MF_MT_FRAME_SIZE, &w, &h);
            d->stride = (int)w;
            d->vh = (int)h;
            d->w = (int)w;
            d->h = (int)h;
            MFVideoArea area;
            if (SUCCEEDED(IMFMediaType_GetBlob(t, &MF_MT_MINIMUM_DISPLAY_APERTURE, (UINT8 *)&area, sizeof area, NULL))) {
                d->w = area.Area.cx; // e.g. 1080 visible in a 1088-high buffer
                d->h = area.Area.cy;
            }
            IMFMediaType_Release(t);
            return hr;
        }
        IMFMediaType_Release(t);
    }
}

qv_wdec *qv_wdec_new(uintptr_t handle, char **err) {
    if (qv_mf_startup(err) < 0) return NULL;
    struct qv_wdec *d = calloc(1, sizeof *d);
    d->handle = handle;
    HRESULT hr = CoCreateInstance(&CLSID_CMSH264DecoderMFT, NULL, CLSCTX_INPROC_SERVER, &IID_IMFTransform,
                                  (void **)&d->mft);
    if (FAILED(hr)) {
        *err = hrerr("no H.264 decoder MFT", hr);
        free(d);
        return NULL;
    }
    ICodecAPI *c = NULL;
    if (SUCCEEDED(IMFTransform_QueryInterface(d->mft, &IID_ICodecAPI, (void **)&c))) {
        setBool(c, &QV_AVLowLatencyMode, TRUE);
        ICodecAPI_Release(c);
    }
    IMFMediaType *in = NULL;
    MFCreateMediaType(&in);
    IMFMediaType_SetGUID(in, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
    IMFMediaType_SetGUID(in, &MF_MT_SUBTYPE, &MFVideoFormat_H264);
    hr = IMFTransform_SetInputType(d->mft, 0, in, 0);
    IMFMediaType_Release(in);
    if (FAILED(hr) || FAILED(hr = setDecOutput(d))) {
        *err = hrerr("configuring the H.264 decoder failed", hr);
        IMFTransform_Release(d->mft);
        free(d);
        return NULL;
    }
    IMFTransform_ProcessMessage(d->mft, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    return d;
}

static HRESULT decPull(struct qv_wdec *d) {
    for (;;) {
        MFT_OUTPUT_STREAM_INFO info = {0};
        IMFTransform_GetOutputStreamInfo(d->mft, 0, &info);
        IMFSample *s = NULL;
        IMFMediaBuffer *b = NULL;
        MFCreateSample(&s);
        MFCreateMemoryBuffer(info.cbSize ? info.cbSize : (DWORD)(d->stride * d->vh * 3 / 2), &b);
        IMFSample_AddBuffer(s, b);
        IMFMediaBuffer_Release(b);
        MFT_OUTPUT_DATA_BUFFER out = {0};
        out.pSample = s;
        DWORD status = 0;
        HRESULT hr = IMFTransform_ProcessOutput(d->mft, 0, 1, &out, &status);
        if (hr == MF_E_TRANSFORM_STREAM_CHANGE) { // first frame, or a new size (layer switch)
            IMFSample_Release(s);
            if (FAILED(hr = setDecOutput(d))) return hr;
            continue;
        }
        if (FAILED(hr)) {
            IMFSample_Release(s);
            return hr == MF_E_TRANSFORM_NEED_MORE_INPUT ? S_OK : hr;
        }
        IMFMediaBuffer *buf = NULL;
        if (SUCCEEDED(IMFSample_ConvertToContiguousBuffer(s, &buf))) {
            BYTE *data;
            if (SUCCEEDED(IMFMediaBuffer_Lock(buf, &data, NULL, NULL))) {
                qv_wpic *p = qv_wpic_new(d->w, d->h);
                qv_nv12_to_bgra(data, d->stride, data + (size_t)d->stride * d->vh, d->stride, d->w, d->h, p->data,
                                p->stride);
                IMFMediaBuffer_Unlock(buf);
                LONGLONG t = 0;
                IMFSample_GetSampleTime(s, &t);
                qvGoWdecOutput(d->handle, p, t / 10); // Go owns p
            }
            IMFMediaBuffer_Release(buf);
        }
        IMFSample_Release(s);
        if (out.pEvents) IMFCollection_Release(out.pEvents);
    }
}

int qv_wdec_decode(qv_wdec *d, const uint8_t *data, int len, int64_t us) {
    coinit();
    IMFMediaBuffer *buf = NULL;
    IMFSample *s = NULL;
    if (FAILED(MFCreateMemoryBuffer(len, &buf))) return -1;
    BYTE *dst;
    IMFMediaBuffer_Lock(buf, &dst, NULL, NULL);
    memcpy(dst, data, len);
    IMFMediaBuffer_Unlock(buf);
    IMFMediaBuffer_SetCurrentLength(buf, len);
    MFCreateSample(&s);
    IMFSample_AddBuffer(s, buf);
    IMFMediaBuffer_Release(buf);
    IMFSample_SetSampleTime(s, us * 10);
    HRESULT hr = IMFTransform_ProcessInput(d->mft, 0, s, 0);
    IMFSample_Release(s);
    if (hr == MF_E_NOTACCEPTING) { // drain, then retry
        decPull(d);
        return qv_wdec_decode(d, data, len, us);
    }
    if (FAILED(hr)) return -1;
    return SUCCEEDED(decPull(d)) ? 0 : -1;
}

void qv_wdec_close(qv_wdec *d) {
    if (!d) return;
    coinit();
    IMFTransform_Release(d->mft);
    free(d);
}

// ---------------------------------------------------------------- webcam

static IMFActivate **enumCams(UINT32 *count) {
    IMFAttributes *a = NULL;
    IMFActivate **devs = NULL;
    *count = 0;
    MFCreateAttributes(&a, 1);
    IMFAttributes_SetGUID(a, &MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE, &MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID);
    MFEnumDeviceSources(a, &devs, count);
    IMFAttributes_Release(a);
    return devs;
}

static void utf8(const WCHAR *w, char *out, int n) { WideCharToMultiByte(CP_UTF8, 0, w, -1, out, n, NULL, NULL); }

char *qv_cam_list(char **err) {
    if (qv_mf_startup(err) < 0) return NULL;
    UINT32 n = 0;
    IMFActivate **devs = enumCams(&n);
    size_t cap = 4096, len = 0;
    char *out = calloc(1, cap);
    for (UINT32 i = 0; i < n; i++) {
        WCHAR *name = NULL, *link = NULL;
        UINT32 l;
        IMFActivate_GetAllocatedString(devs[i], &MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME, &name, &l);
        IMFActivate_GetAllocatedString(devs[i], &MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK, &link, &l);
        char n8[256] = "", l8[1024] = "";
        if (name) utf8(name, n8, sizeof n8);
        if (link) utf8(link, l8, sizeof l8);
        char line[1400];
        int ll = snprintf(line, sizeof line, "%s\t%s\n", l8, n8);
        if (len + ll + 1 > cap) out = realloc(out, cap *= 2);
        memcpy(out + len, line, ll + 1);
        len += ll;
        CoTaskMemFree(name);
        CoTaskMemFree(link);
        IMFActivate_Release(devs[i]);
    }
    CoTaskMemFree(devs);
    return out;
}

struct qv_wcam {
    IMFSourceReader *reader;
    IMFMediaSource *source;
    HANDLE thread;
    volatile LONG stop;
    uintptr_t handle;
    int w, h;
    LONG stride; // negative = bottom-up
};

static DWORD WINAPI camLoop(LPVOID arg) {
    struct qv_wcam *c = arg;
    coinit();
    while (!c->stop) {
        DWORD stream, flags = 0;
        LONGLONG t;
        IMFSample *s = NULL;
        HRESULT hr = IMFSourceReader_ReadSample(c->reader, MF_SOURCE_READER_FIRST_VIDEO_STREAM, 0, &stream, &flags, &t, &s);
        if (FAILED(hr) || (flags & MF_SOURCE_READERF_ENDOFSTREAM)) break;
        if (!s) continue;
        IMFMediaBuffer *buf = NULL;
        if (SUCCEEDED(IMFSample_ConvertToContiguousBuffer(s, &buf))) {
            BYTE *data;
            if (SUCCEEDED(IMFMediaBuffer_Lock(buf, &data, NULL, NULL))) {
                qv_wpic *p = qv_wpic_new(c->w, c->h);
                LONG st = c->stride ? c->stride : c->w * 4;
                for (int y = 0; y < c->h; y++) {
                    const BYTE *row = st > 0 ? data + (size_t)y * st : data + (size_t)(c->h - 1 - y) * (-st);
                    memcpy(p->data + (size_t)y * p->stride, row, (size_t)c->w * 4);
                }
                IMFMediaBuffer_Unlock(buf);
                qvGoWcapFrame(c->handle, p, nowUS()); // Go owns p
            }
            IMFMediaBuffer_Release(buf);
        }
        IMFSample_Release(s);
    }
    return 0;
}

qv_wcam *qv_cam_start(const char *symlink, int w, int h, int fps, uintptr_t handle, char **err, int *denied) {
    *denied = 0;
    if (qv_mf_startup(err) < 0) return NULL;
    UINT32 n = 0;
    IMFActivate **devs = enumCams(&n);
    IMFActivate *pick = NULL;
    for (UINT32 i = 0; i < n; i++) {
        WCHAR *link = NULL;
        UINT32 l;
        char l8[1024] = "";
        if (SUCCEEDED(IMFActivate_GetAllocatedString(devs[i], &MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK, &link, &l))) {
            utf8(link, l8, sizeof l8);
            CoTaskMemFree(link);
        }
        if (!pick && (!symlink[0] || strcmp(l8, symlink) == 0)) pick = devs[i];
        else IMFActivate_Release(devs[i]);
    }
    CoTaskMemFree(devs);
    if (!pick) {
        *err = strdup("no camera found");
        return NULL;
    }
    struct qv_wcam *c = calloc(1, sizeof *c);
    c->handle = handle;
    HRESULT hr = IMFActivate_ActivateObject(pick, &IID_IMFMediaSource, (void **)&c->source);
    IMFActivate_Release(pick);
    if (FAILED(hr)) {
        *denied = hr == E_ACCESSDENIED;
        *err = hrerr("opening the camera failed", hr);
        free(c);
        return NULL;
    }
    IMFAttributes *a = NULL;
    MFCreateAttributes(&a, 1);
    IMFAttributes_SetUINT32(a, &MF_SOURCE_READER_ENABLE_VIDEO_PROCESSING, TRUE); // converts to RGB32 for us
    hr = MFCreateSourceReaderFromMediaSource(c->source, a, &c->reader);
    IMFAttributes_Release(a);
    if (FAILED(hr)) {
        *denied = hr == E_ACCESSDENIED;
        *err = hrerr("camera reader failed", hr);
        IMFMediaSource_Release(c->source);
        free(c);
        return NULL;
    }
    // Pick the native size closest to what was asked (at a frame rate that covers fps).
    UINT32 bw = 0, bh = 0;
    double best = 1e18;
    for (DWORD i = 0;; i++) {
        IMFMediaType *t = NULL;
        if (FAILED(IMFSourceReader_GetNativeMediaType(c->reader, MF_SOURCE_READER_FIRST_VIDEO_STREAM, i, &t))) break;
        UINT32 tw = 0, th = 0, num = 0, den = 1;
        MFGetAttributeSize((IMFAttributes *)t, &MF_MT_FRAME_SIZE, &tw, &th);
        MFGetAttributeRatio((IMFAttributes *)t, &MF_MT_FRAME_RATE, &num, &den);
        double score = fabs((double)tw * th - (double)w * h) + ((double)num / (den ? den : 1) + 0.5 < fps ? 1e12 : 0);
        if (score < best) {
            best = score;
            bw = tw;
            bh = th;
        }
        IMFMediaType_Release(t);
    }
    IMFMediaType *rgb = NULL;
    MFCreateMediaType(&rgb);
    IMFMediaType_SetGUID(rgb, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
    IMFMediaType_SetGUID(rgb, &MF_MT_SUBTYPE, &MFVideoFormat_RGB32);
    if (bw && bh) MFSetAttributeSize((IMFAttributes *)rgb, &MF_MT_FRAME_SIZE, bw, bh);
    MFSetAttributeRatio((IMFAttributes *)rgb, &MF_MT_FRAME_RATE, fps, 1);
    hr = IMFSourceReader_SetCurrentMediaType(c->reader, MF_SOURCE_READER_FIRST_VIDEO_STREAM, NULL, rgb);
    IMFMediaType_Release(rgb);
    IMFMediaType *cur = NULL;
    if (FAILED(hr) || FAILED(IMFSourceReader_GetCurrentMediaType(c->reader, MF_SOURCE_READER_FIRST_VIDEO_STREAM, &cur))) {
        *err = hrerr("the camera can't deliver RGB32", hr);
        IMFSourceReader_Release(c->reader);
        IMFMediaSource_Release(c->source);
        free(c);
        return NULL;
    }
    UINT32 cw = 0, ch = 0, st = 0;
    MFGetAttributeSize((IMFAttributes *)cur, &MF_MT_FRAME_SIZE, &cw, &ch);
    if (SUCCEEDED(IMFMediaType_GetUINT32(cur, &MF_MT_DEFAULT_STRIDE, &st))) c->stride = (LONG)st;
    IMFMediaType_Release(cur);
    c->w = (int)cw;
    c->h = (int)ch;
    c->thread = CreateThread(NULL, 0, camLoop, c, 0, NULL);
    return c;
}

void qv_cam_stop(qv_wcam *c) {
    if (!c) return;
    InterlockedExchange(&c->stop, 1);
    WaitForSingleObject(c->thread, 3000);
    CloseHandle(c->thread);
    IMFSourceReader_Release(c->reader);
    IMFMediaSource_Shutdown(c->source);
    IMFMediaSource_Release(c->source);
    free(c);
}
