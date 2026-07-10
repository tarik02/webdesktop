#pragma once

#include <libei.h>
#include <stdbool.h>

void webdesktop_ei_bind_capabilities(struct ei_seat *seat, bool pointer,
                                     bool keyboard);
