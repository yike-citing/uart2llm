#pragma once
#include <stdbool.h>
#include <stdint.h>
#include <stddef.h>
#include "esp_err.h"
#include "cJSON.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"

enum {
    U2_HELLO = 1,
    U2_HELLO_ACK,
    U2_ACK,
    U2_OPEN,
    U2_OPENED,
    U2_DATA,
    U2_CLOSE,
    U2_RPC,
    U2_RPC_REPLY,
    U2_ERROR,
    U2_BUSY,
    U2_ABORT
};
#define U2_MAX_PAYLOAD 4096
#define U2_CHANNELS 8
#define U2_TELEMETRY_CHANNEL 5
#define U2_LOG_CHANNEL 6
#define U2_OTA_CHANNEL 7
#define U2_IS_DATA_CHANNEL(c) ((c) >= 1 && (c) <= 4)
#if CONFIG_U2_USB_VALIDATION
#define U2_TRANSPORT_NAME "usb_serial_jtag_validation"
#define U2_UART_ACTIVE false
#else
#define U2_TRANSPORT_NAME "uart"
#define U2_UART_ACTIVE true
#endif
#define U2_DATA_CHUNK 1024
typedef struct {
    uint32_t session, seq;
    uint16_t len;
    uint8_t kind, channel;
    uint8_t data[U2_MAX_PAYLOAD];
} u2_message;
typedef struct {
    uint64_t rx_bytes, tx_bytes, rx_frames, tx_frames, crc_errors, framing_errors, retries,
        duplicates, queue_full, resets;
} u2_link_stats;
extern volatile uint32_t u2_session;
extern volatile bool u2_authenticated;
extern u2_link_stats u2_stats;
u2_link_stats u2_stats_snapshot(void);
extern QueueHandle_t u2_rx[U2_CHANNELS];
void u2_link_init(void);
// Frame payloads never back DMA/ISR structures and belong in the PSRAM budget.
void *u2_buffer_alloc(size_t length);
bool u2_send(uint8_t channel, uint8_t kind, const void *data, size_t length, uint32_t session);
// Lock-free cancellation check for DATA waiting on a congested link queue.
bool u2_network_stream_valid(uint8_t channel, uint32_t session);
esp_err_t u2_network_reconnect(void);
bool u2_control_idle(void);
void u2_link_reset(void);
esp_err_t u2_uart_config(int baud, bool flow);
void u2_network_init(void);
void u2_network_reset(void);
esp_err_t u2_network_config(cJSON *config);
cJSON *u2_network_state(void);
bool u2_network_ready(void);
void u2_network_cancel(uint8_t channel, uint32_t session, uint32_t sequence);
void u2_network_abort(uint8_t channel, uint32_t session, uint32_t open_sequence);
esp_err_t u2_target_set(const char *host, int port);
cJSON *u2_wifi_scan(void);
void u2_management_init(void);
void u2_management_task(void *arg);
void u2_log(const char *event, const char *detail);
int u2_config_int(const char *key);
void u2_maintenance_tick(void);
