//go:build windows && cgo

// Windows.Graphics.Capture (Windows 10 1903+): monitors and windows into
// a D3D11 frame pool; frames are copied to BGRA through a staging texture.
// The pool only gets a new frame when the content changed, so a static
// screen costs (almost) nothing.
//
// mingw-w64 ships only part of the WinRT capture headers, so the
// interfaces used here are declared below from the Windows SDK's IDL
// (windows.graphics.capture.idl): IIDs and vtable order as published.
#define COBJMACROS
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <d3d11.h>
#include <dwmapi.h>
#include <dxgi1_2.h>
#include <inspectable.h>
#include <roapi.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <winstring.h>
#include "media_windows.h"

extern void qvGoWcapFrame(uintptr_t handle, qv_wpic *p, int64_t us);

typedef struct {
    INT32 Width, Height;
} QVSizeInt32;

// ---- WinRT interfaces (C ABI) ----

#define IINSPECTABLE_METHODS(T)                                                                                       \
    HRESULT(STDMETHODCALLTYPE *QueryInterface)(T * This, REFIID riid, void **ppv);                                     \
    ULONG(STDMETHODCALLTYPE *AddRef)(T * This);                                                                        \
    ULONG(STDMETHODCALLTYPE *Release)(T * This);                                                                       \
    HRESULT(STDMETHODCALLTYPE *GetIids)(T * This, ULONG * count, IID * *iids);                                         \
    HRESULT(STDMETHODCALLTYPE *GetRuntimeClassName)(T * This, HSTRING * name);                                        \
    HRESULT(STDMETHODCALLTYPE *GetTrustLevel)(T * This, TrustLevel * level);

typedef struct QVCaptureItem QVCaptureItem;
typedef struct {
    IINSPECTABLE_METHODS(QVCaptureItem)
    HRESULT(STDMETHODCALLTYPE *get_DisplayName)(QVCaptureItem *This, HSTRING *v);
    HRESULT(STDMETHODCALLTYPE *get_Size)(QVCaptureItem *This, QVSizeInt32 *v);
    void *add_Closed;
    void *remove_Closed;
} QVCaptureItemVtbl;
struct QVCaptureItem {
    const QVCaptureItemVtbl *lpVtbl;
};

typedef struct QVSession QVSession;
typedef struct {
    IINSPECTABLE_METHODS(QVSession)
    HRESULT(STDMETHODCALLTYPE *StartCapture)(QVSession *This);
} QVSessionVtbl;
struct QVSession {
    const QVSessionVtbl *lpVtbl;
};

typedef struct QVSession2 QVSession2;
typedef struct {
    IINSPECTABLE_METHODS(QVSession2)
    HRESULT(STDMETHODCALLTYPE *get_IsCursorCaptureEnabled)(QVSession2 *This, boolean *v);
    HRESULT(STDMETHODCALLTYPE *put_IsCursorCaptureEnabled)(QVSession2 *This, boolean v);
} QVSession2Vtbl;
struct QVSession2 {
    const QVSession2Vtbl *lpVtbl;
};

// IGraphicsCaptureSession3 (Windows 11; UniversalApiContract 12): whether
// Windows draws its yellow "being captured" border. Asked for thumbnails
// only (a share keeps it). Where the interface or the setting isn't there,
// the border stays, nothing else changes.
typedef struct QVSession3 QVSession3;
typedef struct {
    IINSPECTABLE_METHODS(QVSession3)
    HRESULT(STDMETHODCALLTYPE *get_IsBorderRequired)(QVSession3 *This, boolean *v);
    HRESULT(STDMETHODCALLTYPE *put_IsBorderRequired)(QVSession3 *This, boolean v);
} QVSession3Vtbl;
struct QVSession3 {
    const QVSession3Vtbl *lpVtbl;
};

typedef struct QVFrame QVFrame;
typedef struct {
    IINSPECTABLE_METHODS(QVFrame)
    HRESULT(STDMETHODCALLTYPE *get_Surface)(QVFrame *This, IInspectable **v);
    HRESULT(STDMETHODCALLTYPE *get_SystemRelativeTime)(QVFrame *This, INT64 *v);
    HRESULT(STDMETHODCALLTYPE *get_ContentSize)(QVFrame *This, QVSizeInt32 *v);
} QVFrameVtbl;
struct QVFrame {
    const QVFrameVtbl *lpVtbl;
};

typedef struct QVFramePool QVFramePool;
typedef struct {
    IINSPECTABLE_METHODS(QVFramePool)
    HRESULT(STDMETHODCALLTYPE *Recreate)(QVFramePool *This, IInspectable *device, INT32 fmt, INT32 n, QVSizeInt32 size);
    HRESULT(STDMETHODCALLTYPE *TryGetNextFrame)(QVFramePool *This, QVFrame **v);
    void *add_FrameArrived;
    void *remove_FrameArrived;
    HRESULT(STDMETHODCALLTYPE *CreateCaptureSession)(QVFramePool *This, QVCaptureItem *item, QVSession **v);
    void *get_DispatcherQueue;
} QVFramePoolVtbl;
struct QVFramePool {
    const QVFramePoolVtbl *lpVtbl;
};

typedef struct QVFramePoolStatics2 QVFramePoolStatics2;
typedef struct {
    IINSPECTABLE_METHODS(QVFramePoolStatics2)
    HRESULT(STDMETHODCALLTYPE *CreateFreeThreaded)(QVFramePoolStatics2 *This, IInspectable *device, INT32 fmt, INT32 n,
                                                   QVSizeInt32 size, QVFramePool **v);
} QVFramePoolStatics2Vtbl;
struct QVFramePoolStatics2 {
    const QVFramePoolStatics2Vtbl *lpVtbl;
};

typedef struct QVSessionStatics QVSessionStatics;
typedef struct {
    IINSPECTABLE_METHODS(QVSessionStatics)
    HRESULT(STDMETHODCALLTYPE *IsSupported)(QVSessionStatics *This, boolean *v);
} QVSessionStaticsVtbl;
struct QVSessionStatics {
    const QVSessionStaticsVtbl *lpVtbl;
};

typedef struct QVItemInterop QVItemInterop;
typedef struct {
    HRESULT(STDMETHODCALLTYPE *QueryInterface)(QVItemInterop *This, REFIID riid, void **ppv);
    ULONG(STDMETHODCALLTYPE *AddRef)(QVItemInterop *This);
    ULONG(STDMETHODCALLTYPE *Release)(QVItemInterop *This);
    HRESULT(STDMETHODCALLTYPE *CreateForWindow)(QVItemInterop *This, HWND w, REFIID iid, void **result);
    HRESULT(STDMETHODCALLTYPE *CreateForMonitor)(QVItemInterop *This, HMONITOR m, REFIID iid, void **result);
} QVItemInteropVtbl;
struct QVItemInterop {
    const QVItemInteropVtbl *lpVtbl;
};

typedef struct QVDxgiAccess QVDxgiAccess;
typedef struct {
    HRESULT(STDMETHODCALLTYPE *QueryInterface)(QVDxgiAccess *This, REFIID riid, void **ppv);
    ULONG(STDMETHODCALLTYPE *AddRef)(QVDxgiAccess *This);
    ULONG(STDMETHODCALLTYPE *Release)(QVDxgiAccess *This);
    HRESULT(STDMETHODCALLTYPE *GetInterface)(QVDxgiAccess *This, REFIID iid, void **p);
} QVDxgiAccessVtbl;
struct QVDxgiAccess {
    const QVDxgiAccessVtbl *lpVtbl;
};

typedef struct QVClosable QVClosable;
typedef struct {
    IINSPECTABLE_METHODS(QVClosable)
    HRESULT(STDMETHODCALLTYPE *Close)(QVClosable *This);
} QVClosableVtbl;
struct QVClosable {
    const QVClosableVtbl *lpVtbl;
};

static const IID IID_QVCaptureItem = {0x79c3f95b, 0x31f7, 0x4ec2, {0xa4, 0x64, 0x63, 0x2e, 0xf5, 0xd3, 0x07, 0x60}};
static const IID IID_QVItemInterop = {0x3628e81b, 0x3cac, 0x4c60, {0xb7, 0xf4, 0x23, 0xce, 0x0e, 0x0c, 0x33, 0x56}};
static const IID IID_QVFramePoolStatics2 = {0x589b103f, 0x6bbc, 0x5df5, {0xa9, 0x91, 0x02, 0xe2, 0x8b, 0x3b, 0x66, 0xd5}};
static const IID IID_QVSessionStatics = {0x2224a540, 0x5974, 0x49aa, {0xb2, 0x32, 0x08, 0x82, 0x53, 0x6f, 0x4c, 0xb5}};
static const IID IID_QVSession2 = {0x2c39ae40, 0x7d2e, 0x5044, {0x80, 0x4e, 0x8b, 0x67, 0x99, 0xd4, 0xcf, 0x9e}};
static const IID IID_QVSession3 = {0xf2cdd966, 0x22ae, 0x5ea1, {0x95, 0x96, 0x3a, 0x28, 0x93, 0x44, 0xc3, 0xbe}};
static const IID IID_QVDxgiAccess = {0xa9b3d012, 0x3df2, 0x4ee3, {0xb8, 0xd1, 0x86, 0x95, 0xf4, 0x57, 0xd3, 0xc1}};
static const IID IID_QVClosable = {0x30d5a829, 0x7fa4, 0x4026, {0x83, 0xbb, 0xd7, 0x5b, 0xae, 0x4e, 0xa9, 0x9e}};
static const IID IID_QVDirect3DDevice = {0xa37624ab, 0x8d5f, 0x4650, {0x9d, 0x3e, 0x9e, 0xae, 0x3d, 0x9b, 0xc6, 0x70}};

// Exported by d3d11.dll (declared in a C++-only SDK header).
extern HRESULT WINAPI CreateDirect3D11DeviceFromDXGIDevice(IDXGIDevice *dxgi, IInspectable **device);

#define DXGI_B8G8R8A8 87 // Windows.Graphics.DirectX.DirectXPixelFormat.B8G8R8A8UIntNormalized

static HRESULT factory(const wchar_t *cls, REFIID iid, void **out) {
    HSTRING s;
    HRESULT hr = WindowsCreateString(cls, (UINT32)wcslen(cls), &s);
    if (FAILED(hr)) return hr;
    hr = RoGetActivationFactory(s, iid, out);
    WindowsDeleteString(s);
    return hr;
}

static char *hrerr2(const char *what, HRESULT hr) {
    char buf[256];
    snprintf(buf, sizeof buf, "%s (HRESULT 0x%08lx)", what, (unsigned long)hr);
    return strdup(buf);
}

static int64_t nowUS2(void) {
    FILETIME ft;
    GetSystemTimePreciseAsFileTime(&ft);
    uint64_t t = ((uint64_t)ft.dwHighDateTime << 32) | ft.dwLowDateTime;
    return (int64_t)(t / 10) - 11644473600000000LL;
}

int qv_wgc_supported(void) {
    RoInitialize(RO_INIT_MULTITHREADED);
    QVSessionStatics *st = NULL;
    if (FAILED(factory(L"Windows.Graphics.Capture.GraphicsCaptureSession", &IID_QVSessionStatics, (void **)&st))) return 0;
    boolean ok = 0;
    st->lpVtbl->IsSupported(st, &ok);
    st->lpVtbl->Release(st);
    return ok ? 1 : 0;
}

// ---------------------------------------------------------------- listing

typedef struct {
    char *out;
    size_t len, cap;
    int index;
} lister;

static void add(lister *l, const char *line) {
    size_t n = strlen(line);
    if (l->len + n + 1 > l->cap) l->out = realloc(l->out, l->cap = (l->cap + n) * 2);
    memcpy(l->out + l->len, line, n + 1);
    l->len += n;
}

static BOOL CALLBACK onMonitor(HMONITOR m, HDC dc, LPRECT r, LPARAM p) {
    lister *l = (lister *)p;
    MONITORINFO mi = {sizeof mi};
    GetMonitorInfoW(m, &mi);
    char line[128];
    snprintf(line, sizeof line, "D\t%d\t%ld\t%ld\t%d\n", l->index++, mi.rcMonitor.right - mi.rcMonitor.left,
             mi.rcMonitor.bottom - mi.rcMonitor.top, (mi.dwFlags & MONITORINFOF_PRIMARY) ? 1 : 0);
    add(l, line);
    return TRUE;
}

static BOOL CALLBACK onWindow(HWND w, LPARAM p) {
    lister *l = (lister *)p;
    if (!IsWindowVisible(w) || GetWindow(w, GW_OWNER) || (GetWindowLongW(w, GWL_EXSTYLE) & WS_EX_TOOLWINDOW)) return TRUE;
    BOOL cloaked = FALSE;
    DwmGetWindowAttribute(w, DWMWA_CLOAKED, &cloaked, sizeof cloaked);
    WCHAR title[256];
    if (cloaked || GetWindowTextW(w, title, 256) == 0) return TRUE;
    RECT r;
    GetWindowRect(w, &r);
    if (r.right - r.left < 64 || r.bottom - r.top < 64) return TRUE;
    char t8[768];
    WideCharToMultiByte(CP_UTF8, 0, title, -1, t8, sizeof t8, NULL, NULL);
    for (char *c = t8; *c; c++)
        if (*c == '\t' || *c == '\n') *c = ' ';
    char line[1024];
    snprintf(line, sizeof line, "W\t%llu\t%ld\t%ld\t%s\n", (unsigned long long)(uintptr_t)w, r.right - r.left,
             r.bottom - r.top, t8);
    add(l, line);
    return TRUE;
}

char *qv_wgc_list(void) {
    lister l = {calloc(1, 4096), 0, 4096, 0};
    EnumDisplayMonitors(NULL, NULL, onMonitor, (LPARAM)&l);
    EnumWindows(onWindow, (LPARAM)&l);
    return l.out;
}

typedef struct {
    int want, i;
    HMONITOR found;
} monFind;

static BOOL CALLBACK findMonitor(HMONITOR m, HDC dc, LPRECT r, LPARAM p) {
    monFind *f = (monFind *)p;
    if (f->i++ == f->want) {
        f->found = m;
        return FALSE;
    }
    return TRUE;
}

// ---------------------------------------------------------------- capture

struct qv_wgc {
    ID3D11Device *dev;
    ID3D11DeviceContext *ctx;
    IInspectable *rtDevice;
    QVCaptureItem *item;
    QVFramePool *pool;
    QVSession *session;
    ID3D11Texture2D *staging;
    int sw, sh; // staging size
    QVSizeInt32 size;
    HANDLE thread;
    volatile LONG stop;
    uintptr_t handle;
    int interval_ms;
};

static void copyFrame(struct qv_wgc *c, QVFrame *f) {
    QVSizeInt32 cs;
    f->lpVtbl->get_ContentSize(f, &cs);
    IInspectable *surface = NULL;
    if (FAILED(f->lpVtbl->get_Surface(f, &surface))) return;
    QVDxgiAccess *acc = NULL;
    ID3D11Texture2D *tex = NULL;
    if (SUCCEEDED(IInspectable_QueryInterface(surface, &IID_QVDxgiAccess, (void **)&acc)) &&
        SUCCEEDED(acc->lpVtbl->GetInterface(acc, &IID_ID3D11Texture2D, (void **)&tex))) {
        D3D11_TEXTURE2D_DESC d;
        ID3D11Texture2D_GetDesc(tex, &d);
        if (!c->staging || c->sw != (int)d.Width || c->sh != (int)d.Height) {
            if (c->staging) ID3D11Texture2D_Release(c->staging);
            d.Usage = D3D11_USAGE_STAGING;
            d.BindFlags = 0;
            d.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
            d.MiscFlags = 0;
            c->staging = NULL;
            ID3D11Device_CreateTexture2D(c->dev, &d, NULL, &c->staging);
            c->sw = (int)d.Width;
            c->sh = (int)d.Height;
        }
        if (c->staging) {
            ID3D11DeviceContext_CopyResource(c->ctx, (ID3D11Resource *)c->staging, (ID3D11Resource *)tex);
            D3D11_MAPPED_SUBRESOURCE m;
            if (SUCCEEDED(ID3D11DeviceContext_Map(c->ctx, (ID3D11Resource *)c->staging, 0, D3D11_MAP_READ, 0, &m))) {
                int w = cs.Width < c->sw ? cs.Width : c->sw, h = cs.Height < c->sh ? cs.Height : c->sh;
                qv_wpic *p = qv_wpic_new(w & ~1, h & ~1);
                for (int y = 0; y < p->h; y++)
                    memcpy(p->data + (size_t)y * p->stride, (BYTE *)m.pData + (size_t)y * m.RowPitch, (size_t)p->w * 4);
                ID3D11DeviceContext_Unmap(c->ctx, (ID3D11Resource *)c->staging, 0);
                qvGoWcapFrame(c->handle, p, nowUS2()); // Go owns p
            }
        }
    }
    if (tex) ID3D11Texture2D_Release(tex);
    if (acc) acc->lpVtbl->Release(acc);
    IInspectable_Release(surface);
    // The pool's size follows the window/monitor: recreate when it changes.
    if (cs.Width != c->size.Width || cs.Height != c->size.Height) {
        c->size = cs;
        c->pool->lpVtbl->Recreate(c->pool, c->rtDevice, DXGI_B8G8R8A8, 2, cs);
    }
}

static DWORD WINAPI wgcLoop(LPVOID arg) {
    struct qv_wgc *c = arg;
    RoInitialize(RO_INIT_MULTITHREADED);
    while (!c->stop) {
        QVFrame *f = NULL;
        if (SUCCEEDED(c->pool->lpVtbl->TryGetNextFrame(c->pool, &f)) && f) {
            copyFrame(c, f);
            QVClosable *cl = NULL;
            if (SUCCEEDED(f->lpVtbl->QueryInterface(f, &IID_QVClosable, (void **)&cl))) {
                cl->lpVtbl->Close(cl);
                cl->lpVtbl->Release(cl);
            }
            f->lpVtbl->Release(f);
            Sleep(c->interval_ms); // fps cap
        } else {
            Sleep(4); // nothing changed
        }
    }
    return 0;
}

static void wgcFree(struct qv_wgc *c) {
    if (c->session) {
        QVClosable *cl = NULL;
        if (SUCCEEDED(c->session->lpVtbl->QueryInterface(c->session, &IID_QVClosable, (void **)&cl))) {
            cl->lpVtbl->Close(cl);
            cl->lpVtbl->Release(cl);
        }
        c->session->lpVtbl->Release(c->session);
    }
    if (c->pool) {
        QVClosable *cl = NULL;
        if (SUCCEEDED(c->pool->lpVtbl->QueryInterface(c->pool, &IID_QVClosable, (void **)&cl))) {
            cl->lpVtbl->Close(cl);
            cl->lpVtbl->Release(cl);
        }
        c->pool->lpVtbl->Release(c->pool);
    }
    if (c->item) c->item->lpVtbl->Release(c->item);
    if (c->rtDevice) IInspectable_Release(c->rtDevice);
    if (c->staging) ID3D11Texture2D_Release(c->staging);
    if (c->ctx) ID3D11DeviceContext_Release(c->ctx);
    if (c->dev) ID3D11Device_Release(c->dev);
    free(c);
}

qv_wgc *qv_wgc_start(int monitor, uintptr_t hwnd, int fps, int cursor, int borderless, uintptr_t handle, char **err) {
    RoInitialize(RO_INIT_MULTITHREADED);
    struct qv_wgc *c = calloc(1, sizeof *c);
    c->handle = handle;
    c->interval_ms = 1000 / (fps > 0 ? fps : 15) * 85 / 100;
    HRESULT hr = D3D11CreateDevice(NULL, D3D_DRIVER_TYPE_HARDWARE, NULL, D3D11_CREATE_DEVICE_BGRA_SUPPORT, NULL, 0,
                                   D3D11_SDK_VERSION, &c->dev, NULL, &c->ctx);
    if (FAILED(hr)) // e.g. a VM or CI runner without a GPU
        hr = D3D11CreateDevice(NULL, D3D_DRIVER_TYPE_WARP, NULL, D3D11_CREATE_DEVICE_BGRA_SUPPORT, NULL, 0,
                               D3D11_SDK_VERSION, &c->dev, NULL, &c->ctx);
    if (FAILED(hr)) {
        *err = hrerr2("no Direct3D 11 device", hr);
        wgcFree(c);
        return NULL;
    }
    IDXGIDevice *dxgi = NULL;
    ID3D11Device_QueryInterface(c->dev, &IID_IDXGIDevice, (void **)&dxgi);
    hr = CreateDirect3D11DeviceFromDXGIDevice(dxgi, &c->rtDevice);
    IDXGIDevice_Release(dxgi);
    if (FAILED(hr)) {
        *err = hrerr2("CreateDirect3D11DeviceFromDXGIDevice failed", hr);
        wgcFree(c);
        return NULL;
    }
    QVItemInterop *interop = NULL;
    hr = factory(L"Windows.Graphics.Capture.GraphicsCaptureItem", &IID_QVItemInterop, (void **)&interop);
    if (FAILED(hr)) {
        *err = hrerr2("Windows.Graphics.Capture is not available (Windows 10 1903+ needed)", hr);
        wgcFree(c);
        return NULL;
    }
    if (hwnd) {
        hr = interop->lpVtbl->CreateForWindow(interop, (HWND)hwnd, &IID_QVCaptureItem, (void **)&c->item);
    } else {
        monFind f = {monitor, 0, NULL};
        EnumDisplayMonitors(NULL, NULL, findMonitor, (LPARAM)&f);
        HMONITOR m = f.found ? f.found : MonitorFromPoint((POINT){0, 0}, MONITOR_DEFAULTTOPRIMARY);
        hr = interop->lpVtbl->CreateForMonitor(interop, m, &IID_QVCaptureItem, (void **)&c->item);
    }
    interop->lpVtbl->Release(interop);
    if (FAILED(hr)) {
        *err = hrerr2("cannot capture that monitor/window", hr);
        wgcFree(c);
        return NULL;
    }
    c->item->lpVtbl->get_Size(c->item, &c->size);
    QVFramePoolStatics2 *st = NULL;
    hr = factory(L"Windows.Graphics.Capture.Direct3D11CaptureFramePool", &IID_QVFramePoolStatics2, (void **)&st);
    if (SUCCEEDED(hr)) {
        hr = st->lpVtbl->CreateFreeThreaded(st, c->rtDevice, DXGI_B8G8R8A8, 2, c->size, &c->pool);
        st->lpVtbl->Release(st);
    }
    if (FAILED(hr) || FAILED(hr = c->pool->lpVtbl->CreateCaptureSession(c->pool, c->item, &c->session))) {
        *err = hrerr2("creating the capture session failed", hr);
        wgcFree(c);
        return NULL;
    }
    QVSession2 *s2 = NULL;
    if (SUCCEEDED(c->session->lpVtbl->QueryInterface(c->session, &IID_QVSession2, (void **)&s2))) {
        s2->lpVtbl->put_IsCursorCaptureEnabled(s2, cursor ? 1 : 0);
        s2->lpVtbl->Release(s2);
    }
    if (borderless) {
        QVSession3 *s3 = NULL;
        if (SUCCEEDED(c->session->lpVtbl->QueryInterface(c->session, &IID_QVSession3, (void **)&s3))) {
            s3->lpVtbl->put_IsBorderRequired(s3, 0);
            s3->lpVtbl->Release(s3);
        }
    }
    if (FAILED(hr = c->session->lpVtbl->StartCapture(c->session))) {
        *err = hrerr2("StartCapture failed", hr);
        wgcFree(c);
        return NULL;
    }
    c->thread = CreateThread(NULL, 0, wgcLoop, c, 0, NULL);
    return c;
}

void qv_wgc_stop(qv_wgc *c) {
    if (!c) return;
    InterlockedExchange(&c->stop, 1);
    WaitForSingleObject(c->thread, 3000);
    CloseHandle(c->thread);
    wgcFree(c);
}
