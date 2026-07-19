#pragma once

#include <libei.h>
#include <stdbool.h>
#include <stdint.h>

struct webdesktop_kwin_text;

void webdesktop_ei_bind_capabilities(struct ei_seat *seat, bool pointer,
                                     bool keyboard);
struct webdesktop_kwin_text *webdesktop_kwin_text_new(void);
bool webdesktop_kwin_text_send(struct webdesktop_kwin_text *text,
                               uint32_t codepoint);
void webdesktop_kwin_text_close(struct webdesktop_kwin_text *text);
