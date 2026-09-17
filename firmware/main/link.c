#include "u2.h"
#include <string.h>
#include <stdlib.h>
#include "driver/uart.h"
#include "driver/usb_serial_jtag.h"
#include "driver/gpio.h"
#include "esp_rom_crc.h"
#include "freertos/semphr.h"
#include "esp_timer.h"
#include "esp_heap_caps.h"

#if CONFIG_U2_USB_VALIDATION
#if !CONFIG_ESP_CONSOLE_NONE
#error "USB validation transport requires ESP_CONSOLE_NONE; console output corrupts binary frames"
#endif
#if CONFIG_ESP_CONSOLE_SECONDARY_USB_SERIAL_JTAG
#error "USB validation transport requires ESP_CONSOLE_SECONDARY_NONE"
#endif
#endif

#define VALID_PIN(p) ((p) >= 0 && (p) <= 48 && !((p) >= 22 && (p) <= 37))
_Static_assert(VALID_PIN(CONFIG_U2_UART_TX) && CONFIG_U2_UART_TX != 46,
               "TX unavailable or reserved for flash/PSRAM");
_Static_assert(VALID_PIN(CONFIG_U2_UART_RX), "RX unavailable or reserved for flash/PSRAM");
_Static_assert(CONFIG_U2_UART_TX != CONFIG_U2_UART_RX, "TX/RX conflict");
_Static_assert(CONFIG_U2_UART_RTS == -1 ||
                   (VALID_PIN(CONFIG_U2_UART_RTS) && CONFIG_U2_UART_RTS != 46),
               "RTS invalid");
_Static_assert(CONFIG_U2_UART_CTS == -1 || VALID_PIN(CONFIG_U2_UART_CTS), "CTS invalid");
_Static_assert(CONFIG_U2_UART_RTS == -1 || (CONFIG_U2_UART_RTS != CONFIG_U2_UART_RX &&
                                            CONFIG_U2_UART_RTS != CONFIG_U2_UART_TX),
               "RTS conflict");
_Static_assert(CONFIG_U2_UART_CTS == -1 || (CONFIG_U2_UART_CTS != CONFIG_U2_UART_RX &&
                                            CONFIG_U2_UART_CTS != CONFIG_U2_UART_TX &&
                                            CONFIG_U2_UART_CTS != CONFIG_U2_UART_RTS),
               "CTS conflict");
#if CONFIG_ESP_CONSOLE_USB_SERIAL_JTAG || CONFIG_U2_USB_VALIDATION
_Static_assert(CONFIG_U2_UART_TX != 19 && CONFIG_U2_UART_TX != 20 && CONFIG_U2_UART_RX != 19 &&
                   CONFIG_U2_UART_RX != 20,
               "USB console pin conflict");
#endif
#if CONFIG_ESP_CONSOLE_UART
_Static_assert(CONFIG_U2_UART_NUM != CONFIG_ESP_CONSOLE_UART_NUM,
               "Console cannot share tunnel UART");
_Static_assert(CONFIG_U2_UART_TX != CONFIG_ESP_CONSOLE_UART_TX_GPIO &&
                   CONFIG_U2_UART_RX != CONFIG_ESP_CONSOLE_UART_TX_GPIO &&
                   CONFIG_U2_UART_RTS != CONFIG_ESP_CONSOLE_UART_TX_GPIO &&
                   CONFIG_U2_UART_CTS != CONFIG_ESP_CONSOLE_UART_TX_GPIO,
               "Console TX GPIO conflict");
_Static_assert(CONFIG_U2_UART_TX != CONFIG_ESP_CONSOLE_UART_RX_GPIO &&
                   CONFIG_U2_UART_RX != CONFIG_ESP_CONSOLE_UART_RX_GPIO &&
                   CONFIG_U2_UART_RTS != CONFIG_ESP_CONSOLE_UART_RX_GPIO &&
                   CONFIG_U2_UART_CTS != CONFIG_ESP_CONSOLE_UART_RX_GPIO,
               "Console RX GPIO conflict");
#endif
#define BOARD_FREE_PIN(p)                                                                          \
    ((p) == -1 || (VALID_PIN(p) && (p) != CONFIG_U2_UART_TX && (p) != CONFIG_U2_UART_RX &&         \
                   (p) != CONFIG_U2_UART_RTS && (p) != CONFIG_U2_UART_CTS))
_Static_assert(BOARD_FREE_PIN(CONFIG_U2_SPI_MOSI) && BOARD_FREE_PIN(CONFIG_U2_SPI_MISO) &&
                   BOARD_FREE_PIN(CONFIG_U2_SPI_CLK) && BOARD_FREE_PIN(CONFIG_U2_SPI_CS),
               "SPI reserved mapping conflicts with hardware/UART");
_Static_assert(CONFIG_U2_SPI_MOSI == -1 || (CONFIG_U2_SPI_MOSI != CONFIG_U2_SPI_MISO &&
                                            CONFIG_U2_SPI_MOSI != CONFIG_U2_SPI_CLK &&
                                            CONFIG_U2_SPI_MOSI != CONFIG_U2_SPI_CS),
               "SPI MOSI conflict");
_Static_assert(CONFIG_U2_SPI_MISO == -1 ||
                   (CONFIG_U2_SPI_MISO != 46 && CONFIG_U2_SPI_MISO != CONFIG_U2_SPI_CLK &&
                    CONFIG_U2_SPI_MISO != CONFIG_U2_SPI_CS),
               "SPI MISO conflict or input-only pin");
_Static_assert(CONFIG_U2_SPI_CLK == -1 || CONFIG_U2_SPI_CLK != CONFIG_U2_SPI_CS,
               "SPI clock/chip-select conflict");
#if CONFIG_ESP_CONSOLE_USB_SERIAL_JTAG || CONFIG_U2_USB_VALIDATION
_Static_assert(CONFIG_U2_UART_RTS != 19 && CONFIG_U2_UART_RTS != 20 && CONFIG_U2_UART_CTS != 19 &&
                   CONFIG_U2_UART_CTS != 20 && CONFIG_U2_SPI_MOSI != 19 &&
                   CONFIG_U2_SPI_MOSI != 20 && CONFIG_U2_SPI_MISO != 19 &&
                   CONFIG_U2_SPI_MISO != 20 && CONFIG_U2_SPI_CLK != 19 && CONFIG_U2_SPI_CLK != 20 &&
                   CONFIG_U2_SPI_CS != 19 && CONFIG_U2_SPI_CS != 20,
               "USB console pin conflict with flow-control/SPI");
#endif

volatile uint32_t u2_session;
volatile bool u2_authenticated;
u2_link_stats u2_stats;
static portMUX_TYPE stats_guard = portMUX_INITIALIZER_UNLOCKED;
static void stats_add(uint64_t *counter, uint64_t value) {
    taskENTER_CRITICAL(&stats_guard);
    *counter += value;
    taskEXIT_CRITICAL(&stats_guard);
}
u2_link_stats u2_stats_snapshot(void) {
    taskENTER_CRITICAL(&stats_guard);
    u2_link_stats s = u2_stats;
    taskEXIT_CRITICAL(&stats_guard);
    return s;
}
QueueHandle_t u2_rx[U2_CHANNELS];
static QueueHandle_t tx[U2_CHANNELS];
static SemaphoreHandle_t uart_lock, reset_lock, acks[U2_CHANNELS];
static portMUX_TYPE state_guard = portMUX_INITIALIZER_UNLOCKED;
static uint32_t rx_next[U2_CHANNELS], tx_next[U2_CHANNELS];
static volatile uint32_t ack_seq[U2_CHANNELS];
static volatile uint32_t busy_seq[U2_CHANNELS];
static volatile bool tx_active[U2_CHANNELS];
// Only the UART receive task sends unsequenced controls. Keep the full-size
// message off its stack; nested 4 KiB structs would overflow the task stack.
static u2_message *rx_control;
static int active_baud = 115200;
static QueueHandle_t psram_queue(unsigned count) {
    StaticQueue_t *control =
        heap_caps_calloc(1, sizeof(*control), MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    uint8_t *buffer =
        heap_caps_malloc(count * sizeof(u2_message), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    configASSERT(control && buffer);
    return xQueueCreateStatic(count, sizeof(u2_message), buffer, control);
}
bool u2_control_idle(void) {
    for (int c = 0; c < U2_CHANNELS; c++) {
        if (!U2_IS_DATA_CHANNEL(c) && (tx_active[c] || uxQueueMessagesWaiting(tx[c]) != 0))
            return false;
    }
    return true;
}
static uint16_t get16(const uint8_t *p) {
    return p[0] | ((uint16_t)p[1] << 8);
}
static uint32_t get32(const uint8_t *p) {
    return get16(p) | ((uint32_t)get16(p + 2) << 16);
}
static void put16(uint8_t *p, uint16_t v) {
    p[0] = v;
    p[1] = v >> 8;
}
static void put32(uint8_t *p, uint32_t v) {
    put16(p, v);
    put16(p + 2, v >> 16);
}

static void wire_send(const u2_message *m) {
    uint8_t *raw = u2_buffer_alloc(22 + m->len),
            *out = u2_buffer_alloc(24 + m->len + (m->len + 22) / 254);
    if (!raw || !out) {
        free(raw);
        free(out);
        return;
    }
    memset(raw, 0, 18);
    put16(raw, 0x3255);
    raw[2] = 1;
    raw[3] = m->kind;
    put32(raw + 4, m->session);
    raw[8] = m->channel;
    put32(raw + 10, m->seq);
    put16(raw + 14, m->len);
    memcpy(raw + 18, m->data, m->len);
    put32(raw + 18 + m->len, esp_rom_crc32_le(0, raw, 18 + m->len));
    size_t code_at = 0, w = 1;
    uint8_t code = 1;
    for (size_t i = 0; i < m->len + 22; i++) {
        if (raw[i] == 0) {
            out[code_at] = code;
            code_at = w++;
            code = 1;
        } else {
            out[w++] = raw[i];
            if (++code == 255) {
                out[code_at] = code;
                code_at = w++;
                code = 1;
            }
        }
    }
    out[code_at] = code;
    out[w++] = 0;
    xSemaphoreTake(uart_lock, portMAX_DELAY);
    // Never block indefinitely on missing CTS. A failed flow-control change must
    // leave the maintenance task able to restore the fixed recovery settings.
    int64_t deadline = esp_timer_get_time() + 1000000 + (int64_t)w * 10000000 / active_baud;
    size_t sent = 0;
    while (sent < w && esp_timer_get_time() < deadline) {
#if CONFIG_U2_USB_VALIDATION
        int n = usb_serial_jtag_write_bytes(out + sent, w - sent, pdMS_TO_TICKS(20));
#else
        int n = uart_tx_chars(CONFIG_U2_UART_NUM, (const char *)out + sent, w - sent);
#endif
        if (n > 0)
            sent += n;
        else
            vTaskDelay(pdMS_TO_TICKS(1));
    }
    xSemaphoreGive(uart_lock);
    stats_add(&u2_stats.tx_bytes, sent);
    stats_add(&u2_stats.tx_frames, 1);
    free(raw);
    free(out);
}
static void control_send(uint8_t kind, uint8_t channel, uint32_t session, uint32_t seq) {
    rx_control->kind = kind;
    rx_control->channel = channel;
    rx_control->session = session;
    rx_control->seq = seq;
    rx_control->len = 0;
    wire_send(rx_control);
}

static void reset_session(uint32_t expected, uint32_t replacement) {
    xSemaphoreTake(reset_lock, portMAX_DELAY);
    if (expected && expected != u2_session) {
        xSemaphoreGive(reset_lock);
        return;
    }
    taskENTER_CRITICAL(&state_guard);
    u2_session = 0;
    taskEXIT_CRITICAL(&state_guard);
    u2_authenticated = false;
    stats_add(&u2_stats.resets, 1);
    u2_network_reset();
    taskENTER_CRITICAL(&state_guard);
    for (int c = 0; c < U2_CHANNELS; c++) {
        rx_next[c] = 1;
        tx_next[c] = 1;
        ack_seq[c] = 0;
        busy_seq[c] = 0;
    }
    taskEXIT_CRITICAL(&state_guard);
    for (int c = 0; c < U2_CHANNELS; c++) {
        if (tx[c])
            xQueueReset(tx[c]);
        if (u2_rx[c])
            xQueueReset(u2_rx[c]);
        if (acks[c])
            xSemaphoreGive(acks[c]);
    }
    taskENTER_CRITICAL(&state_guard);
    u2_session = replacement;
    taskEXIT_CRITICAL(&state_guard);
    xSemaphoreGive(reset_lock);
}
void u2_link_reset(void) {
    reset_session(0, 0);
}

static void tx_worker(void *arg) {
    int c = (intptr_t)arg;
    u2_message *m = u2_buffer_alloc(sizeof(*m));
    configASSERT(m);
    for (;;) {
        xQueueReceive(tx[c], m, portMAX_DELAY);
        if (!m->session || m->session != u2_session)
            continue;
        tx_active[c] = true;
        taskENTER_CRITICAL(&state_guard);
        m->seq = tx_next[c];
        taskEXIT_CRITICAL(&state_guard);
        bool ok = false;
        for (int attempt = 0; attempt < 10 && m->session == u2_session; attempt++) {
            if (attempt) {
                stats_add(&u2_stats.retries, 1);
            }
            wire_send(m);
            int64_t deadline = esp_timer_get_time() + 500000;
            bool busy_seen = false;
            do {
                if (ack_seq[c] == m->seq) {
                    ok = true;
                    break;
                }
                if (!busy_seen && busy_seq[c] == m->seq) {
                    busy_seen = true;
                    // Keep lost-ACK recovery at 500 ms, but probe a live peer
                    // with exhausted receive credit no faster than every 20 ms.
                    deadline = esp_timer_get_time() + 20000;
                }
                xSemaphoreTake(acks[c], pdMS_TO_TICKS(20));
            } while (esp_timer_get_time() < deadline && m->session == u2_session);
            if (ok)
                break;
            if (busy_seq[c] == m->seq) {
                busy_seq[c] = 0;
                attempt = -1;
            }
        }
        tx_active[c] = false;
        bool reset = false;
        taskENTER_CRITICAL(&state_guard);
        if (m->session == u2_session) {
            if (ok)
                reset = ++tx_next[c] == 0;
            else
                reset = true;
        }
        taskEXIT_CRITICAL(&state_guard);
        if (reset)
            reset_session(m->session, 0);
    }
}
bool u2_send(uint8_t c, uint8_t kind, const void *data, size_t len, uint32_t session) {
    if (c >= U2_CHANNELS || len > U2_MAX_PAYLOAD || !session || session != u2_session)
        return false;
    u2_message *m = u2_buffer_alloc(sizeof(*m));
    if (!m)
        return false;
    *m = (u2_message){.session = session, .kind = kind, .channel = c, .len = len};
    if (len)
        memcpy(m->data, data, len);
    bool ok = false;
    while (session == u2_session) {
        if (kind == U2_DATA && !u2_network_stream_valid(c, session))
            break;
        if (xQueueSend(tx[c], m, pdMS_TO_TICKS(100)) == pdTRUE) {
            ok = true;
            break;
        }
    }
    free(m);
    return ok;
}
static bool accept_session_frame(uint8_t *raw, uint16_t len) {
    uint8_t kind = raw[3], c = raw[8];
    uint32_t session = get32(raw + 4), seq = get32(raw + 10);
    if (!session || session != u2_session)
        return false;
    stats_add(&u2_stats.rx_frames, 1);
    if (kind == U2_ACK) {
        if (len)
            return false;
        ack_seq[c] = seq;
        xSemaphoreGive(acks[c]);
        return false;
    }
    if (kind == U2_BUSY) {
        if (len)
            return false;
        busy_seq[c] = seq;
        xSemaphoreGive(acks[c]);
        return false;
    }
    if (kind == U2_ABORT) {
        if (!len && U2_IS_DATA_CHANNEL(c))
            u2_network_abort(c, session, seq);
        return false;
    }
    if (kind < U2_OPEN || kind > U2_ERROR || seq > rx_next[c])
        return false;
    if (U2_IS_DATA_CHANNEL(c)) {
        if (kind != U2_OPEN && kind != U2_DATA && kind != U2_CLOSE)
            return false;
    } else if (kind != U2_RPC) {
        return false;
    }
    if (!seq || (kind == U2_DATA && len > U2_DATA_CHUNK))
        return false;
    if (seq == rx_next[c]) {
        u2_message *m = u2_buffer_alloc(sizeof(*m));
        if (!m)
            return false;
        *m = (u2_message){.kind = kind, .session = session, .channel = c, .seq = seq, .len = len};
        memcpy(m->data, raw + 18, len);
        bool accepted =
            !(U2_IS_DATA_CHANNEL(c) && kind == U2_DATA && uxQueueMessagesWaiting(u2_rx[c]) >= 4) &&
            xQueueSend(u2_rx[c], m, 0) == pdTRUE;
        free(m);
        if (!accepted) {
            stats_add(&u2_stats.queue_full, 1);
            control_send(U2_BUSY, c, session, seq);
            return false;
        }
        if (kind == U2_CLOSE && U2_IS_DATA_CHANNEL(c))
            u2_network_cancel(c, session, seq);
        if (++rx_next[c] == 0)
            return true;
    } else
        stats_add(&u2_stats.duplicates, 1);
    control_send(U2_ACK, c, session, seq);
    return false;
}
static void receive_frame(uint8_t *raw, size_t n) {
    if (n < 22 || get16(raw) != 0x3255 || raw[2] != 1 || raw[8] >= U2_CHANNELS || raw[9] ||
        get16(raw + 16)) {
        stats_add(&u2_stats.framing_errors, 1);
        return;
    }
    uint16_t len = get16(raw + 14);
    if (len > U2_MAX_PAYLOAD || n != len + 22) {
        stats_add(&u2_stats.framing_errors, 1);
        return;
    }
    if (get32(raw + n - 4) != esp_rom_crc32_le(0, raw, n - 4)) {
        stats_add(&u2_stats.crc_errors, 1);
        return;
    }
    uint32_t session = get32(raw + 4);
    if (raw[3] == U2_HELLO && raw[8] == 0 && get32(raw + 10) == 0 && !len && session) {
        if (session != u2_session)
            reset_session(0, session);
        control_send(U2_HELLO_ACK, 0, session, 0);
        return;
    }
    xSemaphoreTake(reset_lock, portMAX_DELAY);
    bool reset = accept_session_frame(raw, len);
    xSemaphoreGive(reset_lock);
    if (reset)
        reset_session(session, 0);
}
static void rx_worker(void *unused) {
    uint8_t *encoded = u2_buffer_alloc(4200), *raw = u2_buffer_alloc(4200), input[128];
    configASSERT(encoded && raw);
    size_t n = 0;
    bool overflow = false;
    for (;;) {
#if CONFIG_U2_USB_VALIDATION
        int got = usb_serial_jtag_read_bytes(input, sizeof(input), pdMS_TO_TICKS(100));
#else
        int got = uart_read_bytes(CONFIG_U2_UART_NUM, input, sizeof(input), pdMS_TO_TICKS(100));
#endif
        if (got > 0)
            stats_add(&u2_stats.rx_bytes, got);
        for (int i = 0; i < got; i++) {
            uint8_t b = input[i];
            if (b) {
                if (n < 4200 && !overflow)
                    encoded[n++] = b;
                else
                    overflow = true;
                continue;
            }
            if (!overflow && n) {
                size_t r = 0, w = 0;
                bool valid = true;
                while (r < n) {
                    uint8_t code = encoded[r++];
                    if (!code || r + code - 1 > n) {
                        valid = false;
                        break;
                    }
                    for (int j = 1; j < code; j++)
                        raw[w++] = encoded[r++];
                    if (code < 255 && r < n)
                        raw[w++] = 0;
                }
                if (valid)
                    receive_frame(raw, w);
                else
                    stats_add(&u2_stats.framing_errors, 1);
            }
            n = 0;
            overflow = false;
        }
        // Session liveness follows reliable traffic. Idle sessions stay usable without artificial
        // disconnects.
    }
}
esp_err_t u2_uart_config(int baud, bool flow) {
#if CONFIG_U2_USB_VALIDATION
    // The host CDC baud setting is cosmetic; never pretend to reconfigure UART.
    return baud == 115200 && !flow ? ESP_OK : ESP_ERR_NOT_SUPPORTED;
#else
    if (flow && (CONFIG_U2_UART_RTS < 0 || CONFIG_U2_UART_CTS < 0))
        return ESP_ERR_INVALID_ARG;
    xSemaphoreTake(uart_lock, portMAX_DELAY);
    if (!flow)
        uart_set_hw_flow_ctrl(CONFIG_U2_UART_NUM, UART_HW_FLOWCTRL_DISABLE, 64);
    uart_wait_tx_done(CONFIG_U2_UART_NUM, pdMS_TO_TICKS(1000));
    esp_err_t err = uart_set_baudrate(CONFIG_U2_UART_NUM, baud);
    if (err == ESP_OK)
        err = uart_set_hw_flow_ctrl(CONFIG_U2_UART_NUM,
                                    flow ? UART_HW_FLOWCTRL_CTS_RTS : UART_HW_FLOWCTRL_DISABLE, 64);
    if (err == ESP_OK)
        active_baud = baud;
    xSemaphoreGive(uart_lock);
    return err;
#endif
}
void u2_link_init(void) {
    rx_control = u2_buffer_alloc(sizeof(*rx_control));
    configASSERT(rx_control);
    uart_lock = xSemaphoreCreateMutex();
    reset_lock = xSemaphoreCreateMutex();
    configASSERT(uart_lock && reset_lock);
#if CONFIG_U2_USB_VALIDATION
    usb_serial_jtag_driver_config_t cfg = {.tx_buffer_size = 8192, .rx_buffer_size = 16384};
    ESP_ERROR_CHECK(usb_serial_jtag_driver_install(&cfg));
#else
    uart_config_t cfg = {.baud_rate = 115200,
                         .data_bits = UART_DATA_8_BITS,
                         .parity = UART_PARITY_DISABLE,
                         .stop_bits = UART_STOP_BITS_1,
                         .flow_ctrl = UART_HW_FLOWCTRL_DISABLE,
                         .source_clk = UART_SCLK_DEFAULT};
    ESP_ERROR_CHECK(uart_param_config(CONFIG_U2_UART_NUM, &cfg));
    ESP_ERROR_CHECK(uart_set_pin(CONFIG_U2_UART_NUM, CONFIG_U2_UART_TX, CONFIG_U2_UART_RX,
                                 CONFIG_U2_UART_RTS, CONFIG_U2_UART_CTS));
    ESP_ERROR_CHECK(uart_driver_install(CONFIG_U2_UART_NUM, 16384, 0, 0, NULL, 0));
#endif
    for (int c = 0; c < U2_CHANNELS; c++) {
        bool data = U2_IS_DATA_CHANNEL(c);
        unsigned rx_capacity = c == 0 ? 8 : data ? 5 : c == U2_LOG_CHANNEL ? 2 : 4;
        unsigned tx_capacity = c == 0 ? 8 : c == U2_LOG_CHANNEL ? 2 : 4;
        u2_rx[c] = psram_queue(rx_capacity);
        tx[c] = psram_queue(tx_capacity);
        acks[c] = xSemaphoreCreateBinary();
        configASSERT(u2_rx[c] && tx[c] && acks[c]);
        rx_next[c] = tx_next[c] = 1;
        const char *task_name = c == 0                ? "control-tx"
                                : data                ? "data-tx"
                                : c == U2_LOG_CHANNEL ? "logs-tx"
                                : c == U2_OTA_CHANNEL ? "ota-tx"
                                                      : "telemetry-tx";
        UBaseType_t priority = c == 0 ? 7 : c == U2_LOG_CHANNEL ? 4 : 5;
        configASSERT(xTaskCreate(tx_worker, task_name, 3072, (void *)(intptr_t)c, priority, NULL) ==
                     pdPASS);
    }
    configASSERT(xTaskCreate(rx_worker, "uart-rx", 8192, NULL, 8, NULL) == pdPASS);
}
