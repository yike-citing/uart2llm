#include "u2.h"
#include <string.h>
#include <stdlib.h>
#include <stdio.h>
#include "esp_timer.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_heap_caps.h"
#include "esp_chip_info.h"
#include "esp_app_desc.h"
#include "esp_ota_ops.h"
#include "esp_mac.h"
#include "esp_core_dump.h"
#include "esp_psram.h"
#include "esp_flash.h"
#include "nvs_flash.h"
#include "nvs.h"
#include "protocomm.h"
#include "protocomm_security2.h"
#include "mbedtls/base64.h"
#include "psa/crypto.h"
#include "freertos/semphr.h"
#include "lwip/inet.h"
#include "lwip/sockets.h"

typedef struct {
    const char *key, *type, *default_string;
    int min, max, default_int;
    bool secret;
    const char *apply;
} field;
static const field fields[] = {
    {"wifi.ssid", "string", "", 0, 32, 0, false, "confirm"},
    {"wifi.password", "string", "", 0, 64, 0, true, "confirm"},
    {"wifi.hostname", "string", "uart2llm", 1, 32, 0, false, "confirm"},
    {"ip.dhcp", "boolean", NULL, 0, 1, 1, false, "confirm"},
    {"ip.address", "string", "192.168.1.50", 7, 15, 0, false, "confirm"},
    {"ip.gateway", "string", "192.168.1.1", 7, 15, 0, false, "confirm"},
    {"ip.netmask", "string", "255.255.255.0", 7, 15, 0, false, "confirm"},
    {"ip.dns", "string", "1.1.1.1", 7, 15, 0, false, "confirm"},
    {"uart.baud", "integer", NULL, 9600, 3000000, 115200, false, "confirm"},
    {"uart.flow_control", "boolean", NULL, 0, 1, 0, false, "confirm"},
    {"tcp.connect_timeout_ms", "integer", NULL, 1000, 120000, 30000, false, "runtime"},
    {"tcp.idle_timeout_ms", "integer", NULL, 1000, 86400000, 300000, false, "runtime"},
    {"telemetry.interval_ms", "integer", NULL, 250, 60000, 1000, false, "runtime"},
    {"logs.level", "integer", NULL, 0, 5, 3, false, "runtime"}};
#define FIELD_COUNT (sizeof(fields) / sizeof(fields[0]))
static cJSON *desired, *effective, *persisted;
static nvs_handle_t storage;
static SemaphoreHandle_t lock;
static uint32_t revision = 1;
static int64_t confirm_deadline, switch_at, restart_at, rollback_at;
static bool applied;
static protocomm_t *pc;
static protocomm_security2_params_t sec_params;
static uint8_t salt[16], verifier[384];
static bool provisioned;
static uint32_t security_session;
typedef struct {
    uint64_t id, time;
    char event[40], detail[100];
} log_item;
static log_item logs[32];
static uint64_t log_next = 1;
static int64_t last_log_response = -500000;
static portMUX_TYPE log_guard = portMUX_INITIALIZER_UNLOCKED;
static esp_ota_handle_t ota_handle;
static const esp_partition_t *ota_partition;
static size_t ota_size, ota_offset;
static uint8_t ota_expected[32];
static psa_hash_operation_t ota_hash = PSA_HASH_OPERATION_INIT;
static bool ota_has_hash;
static uint32_t ota_session;
static cJSON *cached_state;
static int64_t sample_at;
static const char *state_keys[] = {"device_id",
                                   "transport",
                                   "sample_time_ms",
                                   "uptime_ms",
                                   "reset_reason",
                                   "revision",
                                   "session",
                                   "authenticated",
                                   "memory.internal_free",
                                   "memory.internal_min_free",
                                   "memory.psram_free",
                                   "memory.largest_internal_block",
                                   "memory.tasks",
                                   "link.rx_bytes",
                                   "link.tx_bytes",
                                   "link.rx_frames",
                                   "link.tx_frames",
                                   "link.crc_errors",
                                   "link.framing_errors",
                                   "link.retries",
                                   "link.duplicates",
                                   "link.queue_full",
                                   "link.resets",
                                   "link.rx_queue_depth",
                                   "network.connected",
                                   "network.disconnect_reason",
                                   "network.reconnects",
                                   "network.rssi_dbm",
                                   "network.channel",
                                   "network.ssid",
                                   "network.address",
                                   "network.gateway",
                                   "network.netmask",
                                   "network.dns",
                                   "network.connections",
                                   "ota.active",
                                   "ota.size",
                                   "ota.offset",
                                   "ota.running_partition",
                                   "ota.image_state",
                                   "storage.used_entries",
                                   "storage.free_entries",
                                   "crash.available",
                                   "crash.size_bytes",
                                   "memory.minimum_task_stack_free",
                                   "network.max_tx_power_dbm",
                                   "network.power_save",
                                   "network.power_save_error",
                                   "network.connections[].channel",
                                   "network.connections[].open",
                                   "network.connections[].phase",
                                   "network.connections[].target",
                                   "network.connections[].rx_bytes",
                                   "network.connections[].tx_bytes",
                                   "network.connections[].last_error",
                                   "network.connections[].tcp_nodelay",
                                   "uart.baud",
                                   "uart.active",
                                   "uart.flow_control",
                                   "uart.controller",
                                   "uart.tx_gpio",
                                   "uart.rx_gpio",
                                   "uart.rts_gpio",
                                   "uart.cts_gpio",
                                   "temperature_celsius",
                                   "validity.wifi.rssi_dbm",
                                   "validity.temperature",
                                   "logs.retained_events"};
static bool ota_active;
static int64_t ota_activity;
static int64_t boot_at;

void u2_log(const char *event, const char *detail) {
    int64_t now = esp_timer_get_time();
    taskENTER_CRITICAL(&log_guard);
    unsigned idx = (log_next - 1) % 32;
    logs[idx].id = log_next++;
    logs[idx].time = now / 1000;
    strlcpy(logs[idx].event, event, sizeof(logs[idx].event));
    strlcpy(logs[idx].detail, detail, sizeof(logs[idx].detail));
    taskEXIT_CRITICAL(&log_guard);
}
static cJSON *defaults(void) {
    cJSON *o = cJSON_CreateObject();
    for (size_t i = 0; i < FIELD_COUNT; i++) {
        const field *f = &fields[i];
        if (!strcmp(f->type, "string"))
            cJSON_AddStringToObject(o, f->key, f->default_string);
        else if (!strcmp(f->type, "boolean"))
            cJSON_AddBoolToObject(o, f->key, f->default_int);
        else
            cJSON_AddNumberToObject(o, f->key, f->default_int);
    }
    return o;
}
int u2_config_int(const char *key) {
    if (!lock)
        return 30000;
    xSemaphoreTakeRecursive(lock, portMAX_DELAY);
    cJSON *v = cJSON_GetObjectItemCaseSensitive(effective, key);
    int n = cJSON_IsNumber(v) ? v->valueint : 0;
    xSemaphoreGiveRecursive(lock);
    return n;
}
static cJSON *redacted(cJSON *source) {
    cJSON *o = cJSON_Duplicate(source, true);
    for (size_t i = 0; i < FIELD_COUNT; i++)
        if (fields[i].secret) {
            cJSON *v = cJSON_GetObjectItemCaseSensitive(source, fields[i].key);
            cJSON_ReplaceItemInObjectCaseSensitive(
                o, fields[i].key, cJSON_CreateBool(cJSON_IsString(v) && v->valuestring[0]));
        }
    return o;
}
static const char *validate(cJSON *o) {
    cJSON *v;
    cJSON_ArrayForEach(v, o) {
        bool known = false;
        for (size_t i = 0; i < FIELD_COUNT; i++)
            if (!strcmp(v->string, fields[i].key))
                known = true;
        if (!known)
            return "Unknown configuration key";
    }
    for (size_t i = 0; i < FIELD_COUNT; i++) {
        const field *f = &fields[i];
        v = cJSON_GetObjectItemCaseSensitive(o, f->key);
        if (!strcmp(f->type, "string")) {
            if (!cJSON_IsString(v) || strlen(v->valuestring) < f->min ||
                strlen(v->valuestring) > f->max)
                return f->key;
        } else if (!strcmp(f->type, "boolean")) {
            if (!cJSON_IsBool(v))
                return f->key;
        } else if (!cJSON_IsNumber(v) || v->valuedouble != v->valueint || v->valueint < f->min ||
                   v->valueint > f->max)
            return f->key;
    }
    const char *password = cJSON_GetObjectItemCaseSensitive(o, "wifi.password")->valuestring;
    size_t plen = strlen(password);
    if (plen && plen < 8)
        return "Wi-Fi password must be empty or 8-64 bytes";
    if (plen == 64) {
        for (size_t i = 0; i < 64; i++)
            if (!((password[i] >= '0' && password[i] <= '9') ||
                  (password[i] >= 'a' && password[i] <= 'f') ||
                  (password[i] >= 'A' && password[i] <= 'F')))
                return "64-byte Wi-Fi key must be hex";
    }
    const char *host = cJSON_GetObjectItemCaseSensitive(o, "wifi.hostname")->valuestring;
    for (size_t i = 0; host[i]; i++)
        if (!((host[i] >= 'a' && host[i] <= 'z') || (host[i] >= 'A' && host[i] <= 'Z') ||
              (host[i] >= '0' && host[i] <= '9') || host[i] == '-'))
            return "Invalid hostname";
    if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(o, "uart.flow_control")) &&
        (CONFIG_U2_UART_RTS < 0 || CONFIG_U2_UART_CTS < 0))
        return "RTS/CTS GPIOs absent in this build";
    if (!cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(o, "ip.dhcp"))) {
        const char *keys[] = {"ip.address", "ip.gateway", "ip.netmask", "ip.dns"};
        struct in_addr ip;
        for (int i = 0; i < 4; i++)
            if (inet_pton(AF_INET, cJSON_GetObjectItemCaseSensitive(o, keys[i])->valuestring,
                          &ip) != 1)
                return keys[i];
    }
    return NULL;
}
static cJSON *config_get(void) {
    cJSON *o = cJSON_CreateObject();
    cJSON_AddNumberToObject(o, "revision", revision);
    cJSON_AddItemToObject(o, "desired", redacted(desired));
    cJSON_AddItemToObject(o, "effective", redacted(effective));
    cJSON_AddItemToObject(o, "persisted", redacted(persisted));
    cJSON_AddBoolToObject(o, "pending", applied);
    cJSON_AddNumberToObject(o, "confirm_deadline_ms", confirm_deadline / 1000);
    // This clock is captured for every config reply, independently of the
    // telemetry interval. The UI anchors the remaining confirmation duration
    // to receipt time, rather than comparing ESP uptime with the PC clock.
    cJSON_AddNumberToObject(o, "current_time_ms", esp_timer_get_time() / 1000);
    cJSON_AddNumberToObject(o, "session", u2_session);
    return o;
}
static cJSON *schema(void) {
    cJSON *a = cJSON_CreateArray();
    cJSON *d = defaults();
    for (size_t i = 0; i < FIELD_COUNT; i++) {
        const field *f = &fields[i];
        cJSON *v = cJSON_CreateObject();
        cJSON_AddStringToObject(v, "key", f->key);
        cJSON_AddStringToObject(v, "type", f->type);
        cJSON_AddNumberToObject(v, "min", f->min);
        cJSON_AddNumberToObject(v, "max", f->max);
        cJSON_AddBoolToObject(v, "secret", f->secret);
        cJSON_AddStringToObject(v, "apply", f->apply);
        cJSON_AddStringToObject(v, "persistence", "nvs-confirmed");
        cJSON_AddStringToObject(v, "scope", "runtime");
        cJSON_AddItemToObject(v, "default",
                              cJSON_Duplicate(cJSON_GetObjectItemCaseSensitive(d, f->key), true));
        if (!strncmp(f->key, "ip.", 3) && strcmp(f->key, "ip.dhcp"))
            cJSON_AddStringToObject(v, "depends_on", "ip.dhcp=false");
        if (!strcmp(f->key, "uart.flow_control"))
            cJSON_AddStringToObject(v, "depends_on", "build.uart.rts>=0 && build.uart.cts>=0");
#if CONFIG_U2_USB_VALIDATION
        if (!strncmp(f->key, "uart.", 5)) {
            cJSON_AddBoolToObject(v, "readOnly", true);
            cJSON_AddBoolToObject(v, "supported", false);
            cJSON_AddStringToObject(
                v, "unavailable_reason",
                "Native USB validation transport has no UART baud/flow control");
        }
#endif
        cJSON_AddItemToArray(a, v);
    }
    cJSON_Delete(d);
    return a;
}
static void rollback(void) {
    applied = false;
    confirm_deadline = 0;
    switch_at = 0;
    cJSON_Delete(desired);
    cJSON_Delete(effective);
    desired = cJSON_Duplicate(persisted, true);
    effective = cJSON_Duplicate(persisted, true);
    u2_network_config(effective);
    u2_uart_config(115200, false);
    cJSON_ReplaceItemInObject(effective, "uart.baud", cJSON_CreateNumber(115200));
    cJSON_ReplaceItemInObject(effective, "uart.flow_control", cJSON_CreateBool(false));
    nvs_erase_key(storage, "candidate");
    nvs_commit(storage);
    u2_log("config.rollback", "Recovered confirmed configuration");
}
static bool save_active(cJSON *o) {
    cJSON *document = cJSON_CreateObject();
    cJSON_AddNumberToObject(document, "revision", revision + 1);
    cJSON_AddItemToObject(document, "values", cJSON_Duplicate(o, true));
    char *json = cJSON_PrintUnformatted(document);
    cJSON_Delete(document);
    if (!json)
        return false;
    esp_err_t err = nvs_set_str(storage, "active", json);
    free(json);
    if (err == ESP_OK)
        err = nvs_commit(storage);
    return err == ESP_OK;
}
static cJSON *capabilities(void) {
    cJSON *o = cJSON_CreateObject();
    cJSON_AddStringToObject(o, "model", "ESP32-S3");
    cJSON_AddStringToObject(o, "firmware", esp_app_get_description()->version);
    cJSON_AddNumberToObject(o, "protocol_version", 1);
    cJSON_AddNumberToObject(o, "security_version", 2);
    cJSON_AddNumberToObject(o, "max_connections", 4);
    cJSON_AddNumberToObject(o, "logical_channels", U2_CHANNELS);
    cJSON_AddNumberToObject(o, "log_read_rate_max_per_second", 2);
    cJSON_AddNumberToObject(o, "max_rpc_payload", 3800);
    cJSON_AddNumberToObject(o, "ota_chunk_bytes", 1024);
    cJSON_AddNumberToObject(o, "ota_max_bytes", 0x600000);
    cJSON_AddStringToObject(o, "transport", U2_TRANSPORT_NAME);
    cJSON_AddBoolToObject(o, "uart_configuration_supported", U2_UART_ACTIVE);
    cJSON_AddBoolToObject(o, "spi", false);
    cJSON_AddBoolToObject(o, "provisioned", provisioned);
    uint32_t flash_size = 0;
    if (esp_flash_get_size(NULL, &flash_size) == ESP_OK)
        cJSON_AddNumberToObject(o, "flash_bytes", flash_size);
    else
        cJSON_AddNullToObject(o, "flash_bytes");
    cJSON_AddNumberToObject(o, "psram_bytes", esp_psram_get_size());
    cJSON *pins = cJSON_AddObjectToObject(o, "build");
    cJSON_AddNumberToObject(pins, "uart.number", CONFIG_U2_UART_NUM);
    cJSON_AddNumberToObject(pins, "uart.tx", CONFIG_U2_UART_TX);
    cJSON_AddNumberToObject(pins, "uart.rx", CONFIG_U2_UART_RX);
    cJSON_AddNumberToObject(pins, "uart.rts", CONFIG_U2_UART_RTS);
    cJSON_AddNumberToObject(pins, "uart.cts", CONFIG_U2_UART_CTS);
    cJSON_AddNumberToObject(pins, "spi.mosi", CONFIG_U2_SPI_MOSI);
    cJSON_AddNumberToObject(pins, "spi.miso", CONFIG_U2_SPI_MISO);
    cJSON_AddNumberToObject(pins, "spi.clk", CONFIG_U2_SPI_CLK);
    cJSON_AddNumberToObject(pins, "spi.cs", CONFIG_U2_SPI_CS);
    return o;
}
static cJSON *state(void) {
    cJSON *o = cJSON_CreateObject();
    uint8_t mac[6];
    esp_read_mac(mac, ESP_MAC_WIFI_STA);
    char id[18];
    snprintf(id, sizeof(id), "%02x:%02x:%02x:%02x:%02x:%02x", mac[0], mac[1], mac[2], mac[3],
             mac[4], mac[5]);
    cJSON_AddStringToObject(o, "device_id", id);
    cJSON_AddStringToObject(o, "transport", U2_TRANSPORT_NAME);
    cJSON_AddNumberToObject(o, "sample_time_ms", esp_timer_get_time() / 1000);
    cJSON_AddNumberToObject(o, "uptime_ms", esp_timer_get_time() / 1000);
    cJSON_AddNumberToObject(o, "reset_reason", esp_reset_reason());
    cJSON_AddNumberToObject(o, "revision", revision);
    cJSON_AddNumberToObject(o, "session", u2_session);
    cJSON_AddBoolToObject(o, "authenticated", u2_authenticated);
    cJSON *uart = cJSON_AddObjectToObject(o, "uart");
    cJSON_AddBoolToObject(uart, "active", U2_UART_ACTIVE);
    cJSON_AddNumberToObject(uart, "baud",
                            cJSON_GetObjectItemCaseSensitive(effective, "uart.baud")->valueint);
    cJSON_AddBoolToObject(
        uart, "flow_control",
        cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(effective, "uart.flow_control")));
    cJSON_AddNumberToObject(uart, "controller", CONFIG_U2_UART_NUM);
    cJSON_AddNumberToObject(uart, "tx_gpio", CONFIG_U2_UART_TX);
    cJSON_AddNumberToObject(uart, "rx_gpio", CONFIG_U2_UART_RX);
    cJSON_AddNumberToObject(uart, "rts_gpio", CONFIG_U2_UART_RTS);
    cJSON_AddNumberToObject(uart, "cts_gpio", CONFIG_U2_UART_CTS);
    cJSON_AddNullToObject(o, "temperature_celsius");
    cJSON *mem = cJSON_AddObjectToObject(o, "memory");
    cJSON_AddNumberToObject(mem, "internal_free", heap_caps_get_free_size(MALLOC_CAP_INTERNAL));
    cJSON_AddNumberToObject(mem, "internal_min_free",
                            heap_caps_get_minimum_free_size(MALLOC_CAP_INTERNAL));
    cJSON_AddNumberToObject(mem, "psram_free", heap_caps_get_free_size(MALLOC_CAP_SPIRAM));
    cJSON_AddNumberToObject(mem, "largest_internal_block",
                            heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL));
    cJSON_AddNumberToObject(mem, "tasks", uxTaskGetNumberOfTasks());
#if CONFIG_FREERTOS_USE_TRACE_FACILITY
    UBaseType_t task_capacity = uxTaskGetNumberOfTasks() + 4;
    TaskStatus_t *task_status = u2_buffer_alloc(task_capacity * sizeof(*task_status));
    if (task_status) {
        UBaseType_t task_count = uxTaskGetSystemState(task_status, task_capacity, NULL);
        uint32_t minimum = UINT32_MAX;
        for (unsigned i = 0; i < task_count; i++)
            if (task_status[i].usStackHighWaterMark < minimum)
                minimum = task_status[i].usStackHighWaterMark;
        if (task_count)
            cJSON_AddNumberToObject(mem, "minimum_task_stack_free", minimum);
        free(task_status);
    }
#endif
    cJSON *link = cJSON_AddObjectToObject(o, "link");
    u2_link_stats link_stats = u2_stats_snapshot();
#define STAT(k) cJSON_AddNumberToObject(link, #k, link_stats.k)
    STAT(rx_bytes);
    STAT(tx_bytes);
    STAT(rx_frames);
    STAT(tx_frames);
    STAT(crc_errors);
    STAT(framing_errors);
    STAT(retries);
    STAT(duplicates);
    STAT(queue_full);
    STAT(resets);
#undef STAT
    cJSON *q = cJSON_AddArrayToObject(link, "rx_queue_depth");
    for (int i = 0; i < U2_CHANNELS; i++)
        cJSON_AddItemToArray(q,
                             cJSON_CreateNumber(u2_rx[i] ? uxQueueMessagesWaiting(u2_rx[i]) : 0));
    cJSON *log_state = cJSON_AddObjectToObject(o, "logs");
    taskENTER_CRITICAL(&log_guard);
    uint64_t retained_logs = log_next > 32 ? 32 : log_next - 1;
    taskEXIT_CRITICAL(&log_guard);
    cJSON_AddNumberToObject(log_state, "retained_events", retained_logs);
    cJSON_AddItemToObject(o, "network", u2_network_state());
    cJSON *update = cJSON_AddObjectToObject(o, "ota");
    cJSON_AddBoolToObject(update, "active", ota_active);
    cJSON_AddNumberToObject(update, "size", ota_size);
    cJSON_AddNumberToObject(update, "offset", ota_offset);
    const esp_partition_t *p = esp_ota_get_running_partition();
    if (p)
        cJSON_AddStringToObject(update, "running_partition", p->label);
    esp_ota_img_states_t img;
    if (p && esp_ota_get_state_partition(p, &img) == ESP_OK)
        cJSON_AddNumberToObject(update, "image_state", img);
    nvs_stats_t ns;
    if (nvs_get_stats(NULL, &ns) == ESP_OK) {
        cJSON *s = cJSON_AddObjectToObject(o, "storage");
        cJSON_AddNumberToObject(s, "used_entries", ns.used_entries);
        cJSON_AddNumberToObject(s, "free_entries", ns.free_entries);
    }
    cJSON *crash = cJSON_AddObjectToObject(o, "crash");
    size_t addr = 0, sz = 0;
    bool has_crash = esp_core_dump_image_get(&addr, &sz) == ESP_OK;
    cJSON_AddBoolToObject(crash, "available", has_crash);
    cJSON_AddNumberToObject(crash, "size_bytes", has_crash ? sz : 0);
    cJSON *valid = cJSON_AddObjectToObject(o, "validity");
    cJSON_AddStringToObject(valid, "wifi.rssi_dbm", "null while disconnected");
    cJSON_AddStringToObject(valid, "temperature",
                            "not sampled: no calibrated temperature sensor configured");
    return o;
}
static void abort_ota(void) {
    if (ota_active) {
        esp_ota_abort(ota_handle);
        psa_hash_abort(&ota_hash);
    }
    ota_active = false;
    ota_size = ota_offset = 0;
}
#if CONFIG_FREERTOS_USE_TRACE_FACILITY
static int compare_task_number(const void *left, const void *right) {
    const TaskStatus_t *a = left, *b = right;
    return (a->xTaskNumber > b->xTaskNumber) - (a->xTaskNumber < b->xTaskNumber);
}
#endif
static cJSON *task_diagnostics(cJSON *params, const char **error) {
#if CONFIG_FREERTOS_USE_TRACE_FACILITY
    cJSON *off = cJSON_GetObjectItemCaseSensitive(params, "offset");
    cJSON *after = cJSON_GetObjectItemCaseSensitive(params, "after_id");
    bool cursor = after != NULL;
    uint32_t after_id = 0;
    if (cursor) {
        if (!cJSON_IsNumber(after) || after->valuedouble < 0 ||
            after->valuedouble > UINT32_MAX ||
            after->valuedouble != (double)(uint32_t)after->valuedouble) {
            *error = "Invalid task ID cursor";
            return NULL;
        }
        after_id = (uint32_t)after->valuedouble;
    }
    int start = cJSON_IsNumber(off) ? off->valueint : 0;
    UBaseType_t capacity = uxTaskGetNumberOfTasks() + 4;
    TaskStatus_t *s = u2_buffer_alloc(capacity * sizeof(*s));
    if (!s) {
        *error = "Task snapshot allocation failed";
        return NULL;
    }
    UBaseType_t count = uxTaskGetSystemState(s, capacity, NULL);
    if (!count || (!cursor && (start < 0 || start > count))) {
        free(s);
        *error = "Task list changed or invalid offset; retry snapshot";
        return NULL;
    }
    // FreeRTOS groups tasks by current scheduling state, so its iteration order
    // changes while callers page through the list. IDs provide stable ordering.
    qsort(s, count, sizeof(*s), compare_task_number);
    // An offset into a fresh snapshot skips survivors when earlier tasks vanish.
    // IDs are monotonic within this boot; resume after the last returned ID.
    if (cursor) {
        start = 0;
        while (start < count && s[start].xTaskNumber <= after_id)
            start++;
    }
    cJSON *o = cJSON_CreateObject(), *a = cJSON_AddArrayToObject(o, "items");
    int end = start + 12;
    if (end > count)
        end = count;
    for (int i = start; i < end; i++) {
        cJSON *v = cJSON_CreateObject();
        cJSON_AddStringToObject(v, "name", s[i].pcTaskName);
        cJSON_AddNumberToObject(v, "id", s[i].xTaskNumber);
        cJSON_AddNumberToObject(v, "state", s[i].eCurrentState);
        cJSON_AddNumberToObject(v, "priority", s[i].uxCurrentPriority);
        cJSON_AddNumberToObject(v, "stack_free_bytes", s[i].usStackHighWaterMark);
        cJSON_AddItemToArray(a, v);
    }
    if (end < count) {
        cJSON_AddNumberToObject(o, "next_offset", end);
        cJSON_AddNumberToObject(o, "next_after_id", s[end - 1].xTaskNumber);
    } else {
        cJSON_AddNullToObject(o, "next_offset");
        cJSON_AddNullToObject(o, "next_after_id");
    }
    free(s);
    return o;
#else
    *error = "Task tracing disabled at build time";
    return NULL;
#endif
}
static cJSON *rpc_execute(const char *method, cJSON *params, const char **error) {
    if (!strcmp(method, "config.reset")) {
        if (applied) {
            *error = "Rollback or confirm current transaction first";
            return NULL;
        }
        cJSON_Delete(desired);
        desired = defaults();
        method = "config.apply";
    }
    if (!strcmp(method, "target.set")) {
        cJSON *h = cJSON_GetObjectItemCaseSensitive(params, "host"),
              *p = cJSON_GetObjectItemCaseSensitive(params, "port");
        if (!cJSON_IsString(h) || !cJSON_IsNumber(p) || p->valuedouble != p->valueint ||
            u2_target_set(h->valuestring, p->valueint) != ESP_OK) {
            *error = "Invalid target hostname/port";
            return NULL;
        }
        return cJSON_CreateTrue();
    }
    if (!strcmp(method, "capabilities"))
        return capabilities();
    if (!strcmp(method, "config.schema"))
        return schema();
    if (!strcmp(method, "state.schema")) {
        cJSON *offset = cJSON_GetObjectItemCaseSensitive(params, "offset"),
              *limit = cJSON_GetObjectItemCaseSensitive(params, "limit");
        int start = cJSON_IsNumber(offset) ? offset->valueint : 0,
            count = cJSON_IsNumber(limit) ? limit->valueint : 12,
            total = sizeof(state_keys) / sizeof(state_keys[0]);
        if (start < 0 || start > total || count < 1 || count > 12) {
            *error = "Invalid schema offset or limit (1-12)";
            return NULL;
        }
        cJSON *o = cJSON_CreateObject(), *a = cJSON_AddArrayToObject(o, "items");
        int end = start + count;
        if (end > total)
            end = total;
        for (int i = start; i < end; i++) {
            const char *k = state_keys[i];
            cJSON *v = cJSON_CreateObject();
            cJSON_AddStringToObject(v, "key", k);
            cJSON_AddStringToObject(v, "source", "esp-idf");
            cJSON_AddStringToObject(v, "unit",
                                    strstr(k, "bytes") || strstr(k, "free") || strstr(k, "block")
                                        ? "bytes"
                                    : strstr(k, "_ms") ? "milliseconds"
                                    : strstr(k, "dbm") ? "dBm"
                                                       : "value");
            cJSON_AddStringToObject(v, "sample_timestamp", "sample_time_ms");
            cJSON_AddStringToObject(v, "validity",
                                    !strncmp(k, "network.", 8)
                                        ? "Requires Wi-Fi association for radio measurements; "
                                          "address may be 0.0.0.0 while disconnected"
                                        : "valid when device snapshot exists");
            cJSON_AddItemToArray(a, v);
        }
        if (end < total)
            cJSON_AddNumberToObject(o, "next_offset", end);
        else
            cJSON_AddNullToObject(o, "next_offset");
        return o;
    }
    if (!strcmp(method, "config.get"))
        return config_get();
    if (!strcmp(method, "config.defaults")) {
        if (applied) {
            *error = "Transaction already applied";
            return NULL;
        }
        cJSON_Delete(desired);
        desired = defaults();
        return config_get();
    }
    if (!strcmp(method, "config.stage")) {
        if (applied) {
            *error = "Confirm or rollback active transaction first";
            return NULL;
        }
        cJSON *values = cJSON_GetObjectItemCaseSensitive(params, "values");
        if (!cJSON_IsObject(values)) {
            *error = "values must be an object";
            return NULL;
        }
#if CONFIG_U2_USB_VALIDATION
        if (cJSON_GetObjectItemCaseSensitive(values, "uart.baud") ||
            cJSON_GetObjectItemCaseSensitive(values, "uart.flow_control")) {
            *error = "UART configuration is unsupported on native USB validation transport";
            return NULL;
        }
#endif
        cJSON *candidate = cJSON_Duplicate(desired, true), *v;
        const char *uartkeys[] = {"uart.baud", "uart.flow_control"};
        for (int i = 0; i < 2; i++)
            if (!cJSON_GetObjectItemCaseSensitive(values, uartkeys[i]))
                cJSON_ReplaceItemInObject(
                    candidate, uartkeys[i],
                    cJSON_Duplicate(cJSON_GetObjectItemCaseSensitive(effective, uartkeys[i]),
                                    true));
        cJSON_ArrayForEach(v, values) {
            cJSON_DeleteItemFromObjectCaseSensitive(candidate, v->string);
            cJSON_AddItemToObject(candidate, v->string, cJSON_Duplicate(v, true));
        }
        *error = validate(candidate);
        if (*error) {
            cJSON_Delete(candidate);
            return NULL;
        }
        cJSON_Delete(desired);
        desired = candidate;
        return config_get();
    }
    if (!strcmp(method, "config.apply")) {
        if (applied) {
            *error = "Already awaiting confirmation";
            return NULL;
        }
#if CONFIG_U2_USB_VALIDATION
        if (cJSON_GetObjectItemCaseSensitive(desired, "uart.baud")->valueint != 115200 ||
            cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(desired, "uart.flow_control"))) {
            *error = "UART configuration is unsupported on native USB validation transport";
            return NULL;
        }
#endif
        char *json = cJSON_PrintUnformatted(desired);
        if (!json) {
            *error = "No memory";
            return NULL;
        }
        esp_err_t e = nvs_set_str(storage, "candidate", json);
        free(json);
        if (e == ESP_OK)
            e = nvs_commit(storage);
        if (e != ESP_OK) {
            *error = "NVS staging failed";
            return NULL;
        }
        bool network_changed = false;
        for (size_t i = 0; i < FIELD_COUNT; i++)
            if ((!strncmp(fields[i].key, "wifi.", 5) || !strncmp(fields[i].key, "ip.", 3)) &&
                !cJSON_Compare(cJSON_GetObjectItemCaseSensitive(effective, fields[i].key),
                               cJSON_GetObjectItemCaseSensitive(desired, fields[i].key), true))
                network_changed = true;
        cJSON *old_baud =
                  cJSON_Duplicate(cJSON_GetObjectItemCaseSensitive(effective, "uart.baud"), true),
              *old_flow = cJSON_Duplicate(
                  cJSON_GetObjectItemCaseSensitive(effective, "uart.flow_control"), true);
        bool uart_changed =
            !cJSON_Compare(old_baud, cJSON_GetObjectItemCaseSensitive(desired, "uart.baud"),
                           true) ||
            !cJSON_Compare(old_flow, cJSON_GetObjectItemCaseSensitive(desired, "uart.flow_control"),
                           true);
        applied = true;
        confirm_deadline = esp_timer_get_time() + 30000000;
        switch_at = uart_changed ? esp_timer_get_time() + 1000000 : 0;
        cJSON_Delete(effective);
        effective = cJSON_Duplicate(desired, true);
        cJSON_ReplaceItemInObject(effective, "uart.baud", old_baud);
        cJSON_ReplaceItemInObject(effective, "uart.flow_control", old_flow);
        if (network_changed && u2_network_config(desired) != ESP_OK) {
            rollback_at = esp_timer_get_time() + 1000000;
            *error = "Network configuration failed; rollback scheduled";
            return NULL;
        }
        esp_log_level_set("*", cJSON_GetObjectItemCaseSensitive(desired, "logs.level")->valueint);
        esp_log_level_set("protocomm", ESP_LOG_WARN);
        esp_log_level_set("security2", ESP_LOG_WARN);
        esp_log_level_set("srp6a", ESP_LOG_WARN);
        u2_log("config.applied", "Awaiting confirmation for 30 seconds");
        return config_get();
    }
    if (!strcmp(method, "config.confirm")) {
        if (!applied || switch_at) {
            *error = "No applied transaction or UART switch not complete";
            return NULL;
        }
        if (cJSON_GetObjectItemCaseSensitive(desired, "wifi.ssid")->valuestring[0] &&
            !u2_network_ready()) {
            *error = "Wi-Fi configuration has not acquired an IP address";
            return NULL;
        }
        if (!save_active(desired)) {
            *error = "Atomic NVS commit failed";
            return NULL;
        }
        revision++;
        cJSON_Delete(persisted);
        persisted = cJSON_Duplicate(desired, true);
        applied = false;
        confirm_deadline = 0;
        nvs_erase_key(storage, "candidate");
        nvs_commit(storage);
        u2_log("config.confirmed", "Configuration persisted");
        return config_get();
    }
    if (!strcmp(method, "config.rollback")) {
        rollback_at = esp_timer_get_time() + 1000000;
        return cJSON_CreateTrue();
    }
    if (!strcmp(method, "state.get")) {
        // A 60-second telemetry interval must not expose pre-change Wi-Fi
        // status during the 30-second confirmation window. Also never serve a
        // snapshot captured before the current serial HELLO session.
        cJSON *cached_session = cJSON_GetObjectItemCaseSensitive(cached_state, "session");
        if (applied || !cJSON_IsNumber(cached_session) ||
            (uint32_t)cached_session->valuedouble != u2_session)
            return state();
        return cJSON_Duplicate(cached_state, true);
    }
    if (!strcmp(method, "tasks.get"))
        return task_diagnostics(params, error);
    if (!strcmp(method, "diagnostics")) {
        cJSON *a = cJSON_GetObjectItemCaseSensitive(params, "action");
        if (cJSON_IsString(a) && !strcmp(a->valuestring, "wifi_reconnect")) {
            esp_err_t err = u2_network_reconnect();
            if (err != ESP_OK) {
                *error = esp_err_to_name(err);
                return NULL;
            }
            u2_log("wifi.reconnect_requested", "Authenticated network recovery requested");
            cJSON *result = cJSON_CreateObject();
            cJSON_AddBoolToObject(result, "accepted", true);
            return result;
        }
        if (cJSON_IsString(a) && !strcmp(a->valuestring, "wifi_scan"))
            return u2_wifi_scan();
        if (!a || (cJSON_IsString(a) &&
                   (!strcmp(a->valuestring, "network") || !strcmp(a->valuestring, "health") ||
                    !strcmp(a->valuestring, "snapshot"))))
            return state();
        *error = "Unknown diagnostics action";
        return NULL;
    }
    if (!strcmp(method, "logs.get")) {
        int64_t now = esp_timer_get_time();
        if (now - last_log_response < 500000) {
            *error = "Log polling is limited to 2 responses per second";
            return NULL;
        }
        last_log_response = now;
        uint64_t after = 0;
        cJSON *v = cJSON_GetObjectItemCaseSensitive(params, "after");
        if (cJSON_IsNumber(v))
            after = v->valuedouble;
        log_item *snapshot = u2_buffer_alloc(sizeof(logs));
        if (!snapshot) {
            *error = "No memory for log snapshot";
            return NULL;
        }
        taskENTER_CRITICAL(&log_guard);
        memcpy(snapshot, logs, sizeof(logs));
        uint64_t end = log_next;
        taskEXIT_CRITICAL(&log_guard);
        cJSON *a = cJSON_CreateArray();
        uint64_t start = end > 32 ? end - 32 : 1;
        for (uint64_t id = start; id < end; id++) {
            if (id <= after)
                continue;
            unsigned idx = (id - 1) % 32;
            cJSON *l = cJSON_CreateObject();
            cJSON_AddNumberToObject(l, "id", snapshot[idx].id);
            cJSON_AddNumberToObject(l, "time_ms", snapshot[idx].time);
            cJSON_AddStringToObject(l, "event", snapshot[idx].event);
            cJSON_AddStringToObject(l, "detail", snapshot[idx].detail);
            cJSON_AddItemToArray(a, l);
            if (cJSON_GetArraySize(a) == 16)
                break;
        }
        free(snapshot);
        return a;
    }
    if (!strcmp(method, "device.restart")) {
        restart_at = esp_timer_get_time() + 1000000;
        return cJSON_CreateTrue();
    }
    if (!strcmp(method, "device.self_test")) {
        if (!heap_caps_check_integrity_all(true)) {
            *error = "Heap integrity failed";
            return NULL;
        }
        if (esp_ota_mark_app_valid_cancel_rollback() != ESP_OK) {
            *error = "OTA mark-valid failed";
            return NULL;
        }
        return cJSON_CreateTrue();
    }
    if (!strcmp(method, "crash.read")) {
        size_t addr = 0, size = 0;
        cJSON *o = cJSON_GetObjectItemCaseSensitive(params, "offset");
        int offset = cJSON_IsNumber(o) ? o->valueint : 0;
        if (esp_core_dump_image_get(&addr, &size) != ESP_OK || offset < 0 || offset > size) {
            *error = "No coredump or invalid offset";
            return NULL;
        }
        const esp_partition_t *part = esp_partition_find_first(
            ESP_PARTITION_TYPE_DATA, ESP_PARTITION_SUBTYPE_DATA_COREDUMP, NULL);
        if (!part) {
            *error = "Coredump partition missing";
            return NULL;
        }
        uint8_t bytes[1024], encoded[1370];
        size_t length = size - offset;
        if (length > sizeof(bytes))
            length = sizeof(bytes);
        if (esp_partition_read(part, addr - part->address + offset, bytes, length) != ESP_OK) {
            *error = "Coredump read failed";
            return NULL;
        }
        size_t n = 0;
        if (mbedtls_base64_encode(encoded, sizeof(encoded), &n, bytes, length) != 0) {
            *error = "Coredump encoding failed";
            return NULL;
        }
        encoded[n] = 0;
        cJSON *r = cJSON_CreateObject();
        cJSON_AddNumberToObject(r, "offset", offset);
        cJSON_AddNumberToObject(r, "size", size);
        cJSON_AddStringToObject(r, "data", (char *)encoded);
        cJSON_AddBoolToObject(r, "eof", offset + length == size);
        return r;
    }
    if (!strcmp(method, "crash.clear")) {
        if (esp_core_dump_image_erase() != ESP_OK) {
            *error = "Coredump erase failed";
            return NULL;
        }
        return cJSON_CreateTrue();
    }
    if (!strcmp(method, "ota.begin")) {
        if (ota_active || applied) {
            *error = "Update or configuration transaction active";
            return NULL;
        }
        cJSON *s = cJSON_GetObjectItemCaseSensitive(params, "size"),
              *h = cJSON_GetObjectItemCaseSensitive(params, "sha256");
        ota_partition = esp_ota_get_next_update_partition(NULL);
        if (!cJSON_IsNumber(s) || s->valuedouble != s->valueint || s->valueint < 256 ||
            !ota_partition || s->valueint > ota_partition->size ||
            (h && (!cJSON_IsString(h) || strlen(h->valuestring) != 64))) {
            *error = "Invalid image size or SHA-256";
            return NULL;
        }
        ota_has_hash = h != NULL;
        if (h)
            for (int i = 0; i < 32; i++) {
                char a = h->valuestring[i * 2], b = h->valuestring[i * 2 + 1];
                int x = (a >= '0' && a <= '9')   ? a - '0'
                        : (a >= 'a' && a <= 'f') ? a - 'a' + 10
                                                 : -1;
                int y = (b >= '0' && b <= '9')   ? b - '0'
                        : (b >= 'a' && b <= 'f') ? b - 'a' + 10
                                                 : -1;
                if (x < 0 || y < 0) {
                    *error = "SHA-256 must be lowercase hexadecimal";
                    return NULL;
                }
                ota_expected[i] = (x << 4) | y;
            }
        if (esp_ota_begin(ota_partition, s->valueint, &ota_handle) != ESP_OK) {
            *error = "OTA partition preparation failed";
            return NULL;
        }
        ota_active = true;
        ota_size = s->valueint;
        ota_offset = 0;
        ota_activity = esp_timer_get_time();
        ota_session = u2_session;
        if (psa_hash_setup(&ota_hash, PSA_ALG_SHA_256) != PSA_SUCCESS) {
            abort_ota();
            *error = "Hash initialization failed";
            return NULL;
        }
        return cJSON_CreateTrue();
    }
    if (!strcmp(method, "ota.write")) {
        cJSON *o = cJSON_GetObjectItemCaseSensitive(params, "offset"),
              *d = cJSON_GetObjectItemCaseSensitive(params, "data");
        if (!ota_active || !cJSON_IsNumber(o) || o->valuedouble != ota_offset ||
            !cJSON_IsString(d)) {
            *error = "No OTA or invalid chunk offset/data";
            return NULL;
        }
        uint8_t bytes[1024];
        size_t len = 0;
        if (mbedtls_base64_decode(bytes, sizeof(bytes), &len, (uint8_t *)d->valuestring,
                                  strlen(d->valuestring)) ||
            !len || ota_offset + len > ota_size) {
            *error = "Invalid base64 or oversized chunk";
            return NULL;
        }
        if (esp_ota_write(ota_handle, bytes, len) != ESP_OK ||
            psa_hash_update(&ota_hash, bytes, len) != PSA_SUCCESS) {
            abort_ota();
            *error = "Flash write or hashing failed";
            return NULL;
        }
        ota_offset += len;
        ota_activity = esp_timer_get_time();
        return cJSON_CreateNumber(ota_offset);
    }
    if (!strcmp(method, "ota.end")) {
        if (!ota_active || ota_offset != ota_size) {
            *error = "Image incomplete";
            return NULL;
        }
        cJSON *h = cJSON_GetObjectItemCaseSensitive(params, "sha256");
        if (!ota_has_hash && !h) {
            *error = "Final SHA-256 required";
            return NULL;
        }
        uint8_t supplied[32];
        if (h) {
            if (!cJSON_IsString(h) || strlen(h->valuestring) != 64) {
                *error = "Invalid final SHA-256";
                return NULL;
            }
            for (int i = 0; i < 32; i++) {
                char pair[3] = {h->valuestring[i * 2], h->valuestring[i * 2 + 1], 0};
                char *end;
                unsigned long n = strtoul(pair, &end, 16);
                if (*end || n > 255) {
                    *error = "Invalid final SHA-256";
                    return NULL;
                }
                supplied[i] = n;
            }
            if (ota_has_hash && memcmp(supplied, ota_expected, 32)) {
                abort_ota();
                *error = "Conflicting SHA-256 values";
                return NULL;
            }
            memcpy(ota_expected, supplied, 32);
        }
        uint8_t hash[32];
        size_t hashlen = 0;
        if (psa_hash_finish(&ota_hash, hash, sizeof(hash), &hashlen) != PSA_SUCCESS ||
            hashlen != 32 || memcmp(hash, ota_expected, 32)) {
            abort_ota();
            *error = "Image SHA-256 mismatch";
            return NULL;
        }
        esp_err_t e = esp_ota_end(ota_handle);
        ota_active = false;
        if (e != ESP_OK) {
            *error = "ESP-IDF image verification failed";
            return NULL;
        }
        if (esp_ota_set_boot_partition(ota_partition) != ESP_OK) {
            *error = "Boot partition selection failed";
            return NULL;
        }
        restart_at = esp_timer_get_time() + 2000000;
        return cJSON_CreateTrue();
    }
    if (!strcmp(method, "ota.abort")) {
        abort_ota();
        return cJSON_CreateTrue();
    }
    *error = "Unknown method";
    return NULL;
}
static esp_err_t rpc_handler(uint32_t session, const uint8_t *input, ssize_t length,
                             uint8_t **output, ssize_t *outlen, void *arg) {
    cJSON *request = cJSON_ParseWithLength((const char *)input, length);
    if (!cJSON_IsObject(request)) {
        cJSON_Delete(request);
        return ESP_ERR_INVALID_ARG;
    }
    cJSON *method = cJSON_GetObjectItemCaseSensitive(request, "method"),
          *params = cJSON_GetObjectItemCaseSensitive(request, "params"),
          *id = cJSON_GetObjectItemCaseSensitive(request, "id");
    if (!cJSON_IsString(method)) {
        cJSON_Delete(request);
        return ESP_ERR_INVALID_ARG;
    }
    u2_authenticated = true;
    const char *error = NULL;
    xSemaphoreTakeRecursive(lock, portMAX_DELAY);
    cJSON *result = rpc_execute(method->valuestring, params, &error);
    xSemaphoreGiveRecursive(lock);
    cJSON *response = cJSON_CreateObject();
    if (id)
        cJSON_AddItemToObject(response, "id", cJSON_Duplicate(id, true));
    if (error) {
        cJSON *e = cJSON_AddObjectToObject(response, "error");
        cJSON_AddStringToObject(e, "code", "device_error");
        cJSON_AddStringToObject(e, "message", error);
    } else
        cJSON_AddItemToObject(response, "result", result ? result : cJSON_CreateNull());
    char *json = cJSON_PrintUnformatted(response);
    cJSON_Delete(request);
    cJSON_Delete(response);
    if (!json)
        return ESP_ERR_NO_MEM;
    if (strlen(json) > 3800) {
        free(json);
        return ESP_ERR_INVALID_SIZE;
    }
    *output = (uint8_t *)json;
    *outlen = strlen(json);
    return ESP_OK;
}
void u2_management_task(void *unused) {
    u2_message *m = u2_buffer_alloc(sizeof(*m));
    configASSERT(m);
    uint8_t *reply = u2_buffer_alloc(U2_MAX_PAYLOAD);
    configASSERT(reply);
    const uint8_t lanes[] = {0, U2_TELEMETRY_CHANNEL, U2_LOG_CHANNEL, U2_OTA_CHANNEL};
    unsigned next_lane = 0;
    for (;;) {
        bool received = false;
        for (unsigned i = 0; i < sizeof(lanes); i++) {
            unsigned lane = (next_lane + i) % sizeof(lanes);
            if (xQueueReceive(u2_rx[lanes[lane]], m, 0) == pdTRUE) {
                next_lane = (lane + 1) % sizeof(lanes);
                received = true;
                break;
            }
        }
        if (!received) {
            vTaskDelay(pdMS_TO_TICKS(5));
            continue;
        }
        if (m->session != u2_session || m->kind != U2_RPC || m->len < 5)
            continue;
        if (!provisioned) {
            memcpy(reply, m->data, 4);
            reply[4] = 1;
            const char *error = "Device has no pairing credentials; provision pairing NVS";
            memcpy(reply + 5, error, strlen(error));
            u2_send(m->channel, U2_RPC_REPLY, reply, 5 + strlen(error), m->session);
            continue;
        }
        if (security_session != m->session) {
            if (security_session)
                protocomm_close_session(pc, security_session);
            security_session = m->session;
            protocomm_open_session(pc, security_session);
            u2_authenticated = false;
        }
        uint8_t namelen = m->data[4];
        if (!namelen || namelen > 40 || 5 + namelen > m->len)
            continue;
        char endpoint[41];
        memcpy(endpoint, m->data + 5, namelen);
        endpoint[namelen] = 0;
        uint8_t *out = NULL;
        ssize_t n = 0;
        esp_err_t err = !strcmp(endpoint, "sec-session") && m->channel != 0
                            ? ESP_ERR_INVALID_ARG
                            : protocomm_req_handle(pc, endpoint, m->session, m->data + 5 + namelen,
                                                   m->len - 5 - namelen, &out, &n);
        if (err != ESP_OK)
            u2_authenticated = false;
        memcpy(reply, m->data, 4);
        reply[4] = err == ESP_OK ? 0 : 1;
        if (err == ESP_OK && n <= U2_MAX_PAYLOAD - 5) {
            memcpy(reply + 5, out, n);
        } else {
            const char *msg = esp_err_to_name(err);
            n = strlen(msg);
            memcpy(reply + 5, msg, n);
            reply[4] = 1;
        }
        free(out);
        u2_send(m->channel, U2_RPC_REPLY, reply, n + 5, m->session);
    }
}
void u2_maintenance_tick(void) {
    int64_t now = esp_timer_get_time();
    xSemaphoreTakeRecursive(lock, portMAX_DELAY);
    if (applied && switch_at && now >= switch_at && u2_control_idle()) {
        int baud = cJSON_GetObjectItemCaseSensitive(desired, "uart.baud")->valueint;
        bool flow = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(desired, "uart.flow_control"));
        if (u2_uart_config(baud, flow) != ESP_OK)
            rollback();
        else {
            cJSON_Delete(effective);
            effective = cJSON_Duplicate(desired, true);
            switch_at = 0;
        }
    }
    if (applied && now >= confirm_deadline) {
        rollback();
    }
    if (rollback_at && now >= rollback_at && u2_control_idle()) {
        rollback_at = 0;
        rollback();
    }
    if (ota_active && (now - ota_activity > 60000000 || ota_session != u2_session)) {
        abort_ota();
        u2_log("ota.aborted", "Session lost or upload timed out");
    }
    if (now >= sample_at) {
        cJSON *next = state();
        cJSON_Delete(cached_state);
        cached_state = next;
        sample_at =
            now + (int64_t)cJSON_GetObjectItemCaseSensitive(effective, "telemetry.interval_ms")
                          ->valueint *
                      1000;
    }
    if (restart_at && now >= restart_at)
        esp_restart();
    const esp_partition_t *p = esp_ota_get_running_partition();
    esp_ota_img_states_t img;
    if (p && esp_ota_get_state_partition(p, &img) == ESP_OK && img == ESP_OTA_IMG_PENDING_VERIFY &&
        now - boot_at > 120000000) {
        u2_log("ota.rollback", "Candidate not confirmed within 120 seconds");
        esp_ota_mark_app_invalid_rollback_and_reboot();
    }
    xSemaphoreGiveRecursive(lock);
}
void u2_management_init(void) {
    lock = xSemaphoreCreateRecursiveMutex();
    configASSERT(lock);
    ESP_ERROR_CHECK(nvs_open("uart2llm", NVS_READWRITE, &storage));
    persisted = defaults();
    size_t length = 0;
    if (nvs_get_str(storage, "active", NULL, &length) == ESP_OK && length < 8192) {
        char *buf = malloc(length);
        if (buf && nvs_get_str(storage, "active", buf, &length) == ESP_OK) {
            cJSON *doc = cJSON_Parse(buf);
            cJSON *v = cJSON_GetObjectItemCaseSensitive(doc, "values"),
                  *r = cJSON_GetObjectItemCaseSensitive(doc, "revision");
            if (cJSON_IsObject(v) && !validate(v)) {
                cJSON_Delete(persisted);
                persisted = cJSON_Duplicate(v, true);
                if (cJSON_IsNumber(r))
                    revision = r->valueint;
            }
            cJSON_Delete(doc);
        }
        free(buf);
    }
    nvs_erase_key(storage, "candidate");
    nvs_commit(storage);
    desired = cJSON_Duplicate(persisted, true);
    effective = cJSON_Duplicate(persisted, true);
    cJSON_ReplaceItemInObject(effective, "uart.baud", cJSON_CreateNumber(115200));
    cJSON_ReplaceItemInObject(effective, "uart.flow_control", cJSON_CreateBool(false));
    u2_network_config(effective);
    nvs_handle_t pair;
    if (nvs_flash_init_partition("pairing") == ESP_OK &&
        nvs_open_from_partition("pairing", "security", NVS_READONLY, &pair) == ESP_OK) {
        size_t sn = sizeof(salt), vn = sizeof(verifier);
        provisioned = nvs_get_blob(pair, "salt", salt, &sn) == ESP_OK && sn == sizeof(salt) &&
                      nvs_get_blob(pair, "verifier", verifier, &vn) == ESP_OK &&
                      vn == sizeof(verifier);
        nvs_close(pair);
    }
    if (provisioned) {
        sec_params = (protocomm_security2_params_t){.salt = (const char *)salt,
                                                    .salt_len = sizeof(salt),
                                                    .verifier = (const char *)verifier,
                                                    .verifier_len = sizeof(verifier)};
        pc = protocomm_new();
        configASSERT(pc);
        ESP_ERROR_CHECK(
            protocomm_set_security(pc, "sec-session", &protocomm_security2, &sec_params));
        ESP_ERROR_CHECK(protocomm_add_endpoint(pc, "rpc", rpc_handler, NULL));
    }
    boot_at = esp_timer_get_time();
    u2_log("device.boot",
           provisioned ? "Awaiting authenticated serial session" : "Pairing NVS missing");
}
