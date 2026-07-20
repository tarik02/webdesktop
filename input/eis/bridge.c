#include "bridge.h"
#include "fake-input-client-protocol.h"

#include <stdlib.h>
#include <string.h>
#include <wayland-client.h>
#include <xkbcommon/xkbcommon.h>

struct webdesktop_kwin_text {
  struct wl_display *display;
  struct org_kde_kwin_fake_input *fake_input;
};

static void webdesktop_kwin_registry_global(void *data,
                                            struct wl_registry *registry,
                                            uint32_t name,
                                            const char *interface,
                                            uint32_t version) {
  struct webdesktop_kwin_text *text = data;
  if (text->fake_input == NULL && version >= 6 &&
      strcmp(interface, org_kde_kwin_fake_input_interface.name) == 0) {
    text->fake_input =
        wl_registry_bind(registry, name, &org_kde_kwin_fake_input_interface, 6);
  }
}

static void webdesktop_kwin_registry_global_remove(void *data,
                                                   struct wl_registry *registry,
                                                   uint32_t name) {
  (void)data;
  (void)registry;
  (void)name;
}

static const struct wl_registry_listener webdesktop_kwin_registry_listener = {
    .global = webdesktop_kwin_registry_global,
    .global_remove = webdesktop_kwin_registry_global_remove,
};

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

struct webdesktop_kwin_text *webdesktop_kwin_text_new(void) {
  struct webdesktop_kwin_text *text = calloc(1, sizeof(*text));
  if (text == NULL) {
    return NULL;
  }
  text->display = wl_display_connect(NULL);
  if (text->display == NULL) {
    free(text);
    return NULL;
  }
  struct wl_registry *registry = wl_display_get_registry(text->display);
  if (registry == NULL ||
      wl_registry_add_listener(registry, &webdesktop_kwin_registry_listener,
                               text) < 0 ||
      wl_display_roundtrip(text->display) < 0) {
    if (registry != NULL) {
      wl_registry_destroy(registry);
    }
    wl_display_disconnect(text->display);
    free(text);
    return NULL;
  }
  wl_registry_destroy(registry);
  if (text->fake_input == NULL) {
    wl_display_disconnect(text->display);
    free(text);
    return NULL;
  }
  org_kde_kwin_fake_input_authenticate(text->fake_input, "webdesktop",
                                       "remote keyboard text input");
  if (wl_display_flush(text->display) < 0) {
    org_kde_kwin_fake_input_destroy(text->fake_input);
    wl_display_disconnect(text->display);
    free(text);
    return NULL;
  }
  return text;
}

bool webdesktop_kwin_text_send(struct webdesktop_kwin_text *text,
                               uint32_t codepoint) {
  if (text == NULL || text->fake_input == NULL) {
    return false;
  }
  xkb_keysym_t keysym = xkb_utf32_to_keysym(codepoint);
  if (keysym == XKB_KEY_NoSymbol) {
    return false;
  }
  org_kde_kwin_fake_input_keyboard_keysym(text->fake_input, keysym,
                                          WL_KEYBOARD_KEY_STATE_PRESSED);
  org_kde_kwin_fake_input_keyboard_keysym(text->fake_input, keysym,
                                          WL_KEYBOARD_KEY_STATE_RELEASED);
  return wl_display_flush(text->display) >= 0;
}

void webdesktop_kwin_text_close(struct webdesktop_kwin_text *text) {
  if (text == NULL) {
    return;
  }
  if (text->fake_input != NULL) {
    org_kde_kwin_fake_input_destroy(text->fake_input);
    wl_display_flush(text->display);
  }
  wl_display_disconnect(text->display);
  free(text);
}
