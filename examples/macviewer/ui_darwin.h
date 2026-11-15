#include <stdint.h>
void qv_ui_run(const char *title, int width, int height); // main thread; returns at quit
void qv_ui_quit(void);
// rects: x, y, w, h per tile (points, top-left origin)
void qv_ui_set_tiles(int n, const uint32_t *ids, const double *rects, const char **labels, const int *visible);
void qv_ui_set_frame(uint32_t track_id, void *pixel_buffer); // retains it while shown
int qv_ui_window_id(void);
