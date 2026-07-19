#include "bridge.h"

void webdesktop_ei_bind_capabilities(struct ei_seat *seat, bool pointer,
                                     bool keyboard) {
  if (pointer && keyboard) {
    ei_seat_bind_capabilities(seat, EI_DEVICE_CAP_POINTER,
                              EI_DEVICE_CAP_POINTER_ABSOLUTE,
                              EI_DEVICE_CAP_BUTTON, EI_DEVICE_CAP_SCROLL,
                              EI_DEVICE_CAP_KEYBOARD, EI_DEVICE_CAP_TEXT, NULL);
  } else if (pointer) {
    ei_seat_bind_capabilities(seat, EI_DEVICE_CAP_POINTER,
                              EI_DEVICE_CAP_POINTER_ABSOLUTE,
                              EI_DEVICE_CAP_BUTTON, EI_DEVICE_CAP_SCROLL, NULL);
  } else if (keyboard) {
    ei_seat_bind_capabilities(seat, EI_DEVICE_CAP_KEYBOARD, EI_DEVICE_CAP_TEXT,
                              NULL);
  }
}
