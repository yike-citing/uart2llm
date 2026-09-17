#include "u2.h"
#include <errno.h>
#include <fcntl.h>
#include <string.h>
#include <unistd.h>
#include <stdio.h>
#include <stdatomic.h>
#include "lwip/sockets.h"
#include "lwip/netdb.h"
#include "esp_wifi.h"
#include "esp_event.h"
#include "esp_netif.h"
#include "esp_mac.h"
#include "esp_timer.h"
#include "esp_check.h"
#include "freertos/semphr.h"

typedef struct {
    uint32_t session, generation;
    char target[260];
} dial_job;
typedef struct {
    int fd;
    uint32_t generation;
    atomic_uint session, open_seq, cancel_session, cancel_seq;
    atomic_uint network_epoch;
    atomic_int last_error;
    bool closing;
    uint64_t read_bytes, written_bytes;
    int64_t activity;
    char target[260];
    SemaphoreHandle_t guard, tx_guard;
    QueueHandle_t dial_queue;
} connection;
static connection slots[4];
static esp_netif_t *sta;
static volatile bool connected;
static volatile int disconnect_reason;
static volatile unsigned reconnects;
static atomic_uint network_epoch;
static esp_err_t power_save_error;
static bool wifi_enabled;
bool u2_network_ready(void) {
    return connected;
}
esp_err_t u2_network_reconnect(void) {
    if (!wifi_enabled)
        return ESP_ERR_INVALID_STATE;
    // An actual disconnect event invalidates old sockets and starts reconnect.
    // No configuration transaction or credential changes are involved.
    esp_err_t err = esp_wifi_disconnect();
    if (err == ESP_ERR_WIFI_NOT_CONNECT)
        err = esp_wifi_connect();
    return err;
}
void u2_network_cancel(uint8_t channel, uint32_t session, uint32_t sequence) {
    if (channel >= 1 && channel <= 4) {
        atomic_store(&slots[channel - 1].cancel_seq, sequence);
        atomic_store(&slots[channel - 1].cancel_session, session);
    }
}
void u2_network_abort(uint8_t channel, uint32_t session, uint32_t open_sequence) {
    if (channel >= 1 && channel <= 4) {
        connection *s = &slots[channel - 1];
        if (s->session == session && s->open_seq == open_sequence)
            u2_network_cancel(channel, session, open_sequence + 1);
    }
}
static bool is_cancelled(connection *s, uint32_t session) {
    return atomic_load(&s->cancel_session) == session && atomic_load(&s->cancel_seq) > s->open_seq;
}
bool u2_network_stream_valid(uint8_t channel, uint32_t session) {
    if (channel < 1 || channel > 4)
        return false;
    connection *s = &slots[channel - 1];
    return s->session == session && !is_cancelled(s, session) &&
           atomic_load(&s->network_epoch) == atomic_load(&network_epoch);
}
static char allowed_target[260];
static uint32_t allowed_session;
static portMUX_TYPE policy_guard = portMUX_INITIALIZER_UNLOCKED;
esp_err_t u2_target_set(const char *host, int port) {
    if (!host || !*host || strlen(host) > 250 || port < 1 || port > 65535 ||
        strpbrk(host, "/\\ \r\n\t"))
        return ESP_ERR_INVALID_ARG;
    char target[260];
    if (strchr(host, ':') && host[0] != '[')
        snprintf(target, sizeof(target), "[%s]:%d", host, port);
    else
        snprintf(target, sizeof(target), "%s:%d", host, port);
    taskENTER_CRITICAL(&policy_guard);
    strlcpy(allowed_target, target, sizeof(allowed_target));
    allowed_session = u2_session;
    taskEXIT_CRITICAL(&policy_guard);
    return ESP_OK;
}

static void close_generation(int i, bool notify, uint32_t generation) {
    connection *s = &slots[i];
    xSemaphoreTake(s->tx_guard, portMAX_DELAY);
    xSemaphoreTake(s->guard, portMAX_DELAY);
    uint32_t session = s->session;
    if (generation != UINT32_MAX && s->generation != generation) {
        xSemaphoreGive(s->guard);
        xSemaphoreGive(s->tx_guard);
        return;
    }
    if (notify && s->closing) {
        xSemaphoreGive(s->guard);
        xSemaphoreGive(s->tx_guard);
        return;
    }
    if (s->fd >= 0) {
        shutdown(s->fd, SHUT_RDWR);
        close(s->fd);
        s->fd = -1;
    }
    s->generation++;
    bool send_close = notify && session && session == u2_session;
    if (send_close) {
        s->closing = true;
    } else {
        s->closing = false;
        s->session = 0;
    }
    xSemaphoreGive(s->guard);
    // Preserve DATA/CLOSE order with the per-channel TX guard, but never hold
    // the socket/state guard while waiting for a slow host to grant credit.
    if (send_close)
        u2_send(i + 1, U2_CLOSE, NULL, 0, session);
    xSemaphoreGive(s->tx_guard);
}
static void close_slot(int i, bool notify) {
    close_generation(i, notify, UINT32_MAX);
}
void u2_network_reset(void) {
    for (int i = 0; i < 4; i++)
        if (slots[i].guard)
            close_slot(i, false);
}
static void wifi_event(void *arg, esp_event_base_t base, int32_t id, void *data) {
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        if (wifi_enabled)
            esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        connected = false;
        disconnect_reason = ((wifi_event_sta_disconnected_t *)data)->reason;
        // Event callbacks must not wait for serial backpressure or socket locks.
        // A generation change cancels every old TCP stream even if Wi-Fi
        // reconnects before its worker next runs; each channel closes itself.
        atomic_fetch_add(&network_epoch, 1);
        u2_log("wifi.disconnected", "Connection lost");
        if (wifi_enabled) {
            reconnects++;
            esp_wifi_connect();
        }
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        connected = true;
        disconnect_reason = 0;
        u2_log("wifi.connected", "IPv4 address acquired");
    }
}
static const char *strval(cJSON *cfg, const char *key) {
    cJSON *v = cJSON_GetObjectItemCaseSensitive(cfg, key);
    return cJSON_IsString(v) ? v->valuestring : "";
}
esp_err_t u2_network_config(cJSON *cfg) {
    wifi_enabled = false;
    esp_wifi_disconnect();
    connected = false;
    atomic_fetch_add(&network_epoch, 1);
    u2_network_reset();
    wifi_config_t wifi = {0};
    memcpy(wifi.sta.ssid, strval(cfg, "wifi.ssid"), strlen(strval(cfg, "wifi.ssid")));
    memcpy(wifi.sta.password, strval(cfg, "wifi.password"), strlen(strval(cfg, "wifi.password")));
    wifi.sta.threshold.authmode = WIFI_AUTH_OPEN;
    wifi.sta.pmf_cfg.capable = true;
    ESP_RETURN_ON_ERROR(esp_wifi_set_config(WIFI_IF_STA, &wifi), "network", "wifi config");
    const char *hostname = strval(cfg, "wifi.hostname");
    ESP_RETURN_ON_ERROR(esp_netif_set_hostname(sta, hostname), "network", "hostname");
    esp_netif_dhcpc_stop(sta);
    if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(cfg, "ip.dhcp"))) {
        esp_netif_dhcpc_start(sta);
    } else {
        esp_netif_ip_info_t ip = {0};
        if (!esp_netif_str_to_ip4(strval(cfg, "ip.address"), &ip.ip) &&
            !esp_netif_str_to_ip4(strval(cfg, "ip.gateway"), &ip.gw) &&
            !esp_netif_str_to_ip4(strval(cfg, "ip.netmask"), &ip.netmask))
            ESP_RETURN_ON_ERROR(esp_netif_set_ip_info(sta, &ip), "network", "static IP");
        else
            return ESP_ERR_INVALID_ARG;
        esp_netif_dns_info_t dns = {.ip.type = ESP_IPADDR_TYPE_V4};
        if (esp_netif_str_to_ip4(strval(cfg, "ip.dns"), &dns.ip.u_addr.ip4) != ESP_OK)
            return ESP_ERR_INVALID_ARG;
        esp_netif_set_dns_info(sta, ESP_NETIF_DNS_MAIN, &dns);
    }
    wifi_enabled = wifi.sta.ssid[0] != 0;
    if (wifi_enabled)
        return esp_wifi_connect();
    return ESP_OK;
}
static int dial_target(char *target, int timeout_ms, int *last_error) {
    *last_error = EINVAL;
    int64_t deadline = esp_timer_get_time() + (int64_t)timeout_ms * 1000;
    char *colon = strrchr(target, ':');
    if (!colon)
        return -1;
    *colon = 0;
    char *host = target, *service = colon + 1;
    if (host[0] == '[') {
        host++;
        char *end = strrchr(host, ']');
        if (!end)
            return -1;
        *end = 0;
    }
    struct addrinfo hints = {.ai_family = AF_UNSPEC, .ai_socktype = SOCK_STREAM}, *list = NULL;
    if (getaddrinfo(host, service, &hints, &list) != 0) {
        *last_error = EHOSTUNREACH;
        return -1;
    }
    int fd = -1;
    if (esp_timer_get_time() >= deadline) {
        *last_error = ETIMEDOUT;
        freeaddrinfo(list);
        return -1;
    }
    for (struct addrinfo *p = list; p; p = p->ai_next) {
        fd = socket(p->ai_family, p->ai_socktype, p->ai_protocol);
        if (fd < 0) {
            *last_error = errno;
            continue;
        }
        int no_delay = 1;
        if (fcntl(fd, F_SETFL, O_NONBLOCK) < 0 ||
            setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &no_delay, sizeof(no_delay)) < 0) {
            *last_error = errno;
            close(fd);
            fd = -1;
            continue;
        }
        int rc = connect(fd, p->ai_addr, p->ai_addrlen);
        if (rc < 0)
            *last_error = errno;
        if (rc < 0 && errno == EINPROGRESS) {
            int64_t remain = deadline - esp_timer_get_time();
            if (remain < 0)
                remain = 0;
            struct timeval tv = {.tv_sec = remain / 1000000, .tv_usec = remain % 1000000};
            fd_set set;
            FD_ZERO(&set);
            FD_SET(fd, &set);
            rc = select(fd + 1, NULL, &set, NULL, &tv);
            int error = 1;
            socklen_t length = sizeof(error);
            if (rc > 0)
                getsockopt(fd, SOL_SOCKET, SO_ERROR, &error, &length);
            *last_error = rc == 0 ? ETIMEDOUT : rc < 0 ? errno : error;
            rc = (rc > 0 && !error) ? 0 : -1;
        }
        if (rc == 0) {
            *last_error = 0;
            break;
        }
        close(fd);
        fd = -1;
        if (esp_timer_get_time() >= deadline)
            break;
    }
    freeaddrinfo(list);
    return fd;
}
static void dial_task(void *arg) {
    int i = (intptr_t)arg;
    connection *s = &slots[i];
    dial_job job;
    for (;;) {
        xQueueReceive(s->dial_queue, &job, portMAX_DELAY);
        int dial_error;
        int fd = dial_target(job.target, u2_config_int("tcp.connect_timeout_ms"), &dial_error);
        xSemaphoreTake(s->tx_guard, portMAX_DELAY);
        xSemaphoreTake(s->guard, portMAX_DELAY);
        bool current = job.session == u2_session && s->session == job.session &&
                       s->generation == job.generation && !s->closing &&
                       u2_network_stream_valid(i + 1, job.session);
        if (current)
            s->last_error = dial_error;
        if (current && fd >= 0) {
            s->fd = fd;
            s->activity = esp_timer_get_time();
        }
        xSemaphoreGive(s->guard);
        if (current) {
            if (fd >= 0)
                u2_send(i + 1, U2_OPENED, NULL, 0, job.session);
            else {
                const char *error = "DNS lookup or TCP connect failed";
                u2_send(i + 1, U2_ERROR, error, strlen(error), job.session);
            }
        } else if (fd >= 0)
            close(fd);
        xSemaphoreGive(s->tx_guard);
    }
}
static void incoming_task(void *arg) {
    int i = (intptr_t)arg;
    connection *s = &slots[i];
    u2_message *m = u2_buffer_alloc(sizeof(*m));
    configASSERT(m);
    while (!u2_rx[i + 1])
        vTaskDelay(pdMS_TO_TICKS(20));
    for (;;) {
        xQueueReceive(u2_rx[i + 1], m, portMAX_DELAY);
        if (m->session != u2_session)
            continue;
        if (m->kind == U2_OPEN) {
            if (!u2_authenticated || !connected || !m->len || m->len > 255 ||
                memchr(m->data, 0, m->len)) {
                const char *error = "Device unpaired, Wi-Fi unavailable, or invalid target";
                u2_send(i + 1, U2_ERROR, error, strlen(error), m->session);
                continue;
            }
            if (s->session) {
                const char *error = "Channel already in use";
                u2_send(i + 1, U2_ERROR, error, strlen(error), m->session);
                continue;
            }
            char target[260];
            memcpy(target, m->data, m->len);
            target[m->len] = 0;
            taskENTER_CRITICAL(&policy_guard);
            bool allowed = allowed_session == m->session && !strcmp(target, allowed_target);
            taskEXIT_CRITICAL(&policy_guard);
            if (!allowed) {
                const char *error = "Target not authorized by management";
                u2_send(i + 1, U2_ERROR, error, strlen(error), m->session);
                continue;
            }
            strlcpy(s->target, target, sizeof(s->target));
            xSemaphoreTake(s->guard, portMAX_DELAY);
            if (m->session != u2_session) {
                xSemaphoreGive(s->guard);
                continue;
            }
            s->session = m->session;
            s->network_epoch = atomic_load(&network_epoch);
            s->open_seq = m->seq;
            s->closing = false;
            s->activity = esp_timer_get_time();
            s->generation++;
            dial_job job = {.session = m->session, .generation = s->generation};
            strlcpy(job.target, target, sizeof(job.target));
            bool queued = xQueueSend(s->dial_queue, &job, 0) == pdTRUE;
            xSemaphoreGive(s->guard);
            if (!queued) {
                const char *error = "DNS worker still cancelling a previous lookup";
                u2_send(i + 1, U2_ERROR, error, strlen(error), m->session);
            }
        } else if (m->kind == U2_DATA) {
            size_t offset = 0;
            int64_t deadline =
                esp_timer_get_time() + (int64_t)u2_config_int("tcp.idle_timeout_ms") * 1000;
            bool failed = false;
            while (offset < m->len && m->session == u2_session &&
                   u2_network_stream_valid(i + 1, m->session)) {
                xSemaphoreTake(s->guard, portMAX_DELAY);
                int fd = s->fd;
                int sent = fd >= 0 ? send(fd, m->data + offset, m->len - offset, MSG_DONTWAIT) : -1;
                int err = errno;
                if (sent > 0) {
                    offset += sent;
                    s->written_bytes += sent;
                    s->activity = esp_timer_get_time();
                }
                xSemaphoreGive(s->guard);
                if (sent <= 0) {
                    if (fd < 0 || (err != EAGAIN && err != EWOULDBLOCK) ||
                        esp_timer_get_time() > deadline) {
                        s->last_error = esp_timer_get_time() > deadline ? ETIMEDOUT : err;
                        failed = true;
                        break;
                    }
                    vTaskDelay(pdMS_TO_TICKS(10));
                }
            }
            if (failed)
                close_slot(i, true);
        } else if (m->kind == U2_CLOSE) {
            if (!s->closing) {
                close_slot(i, true);
                if (!s->session)
                    u2_send(i + 1, U2_CLOSE, NULL, 0, m->session);
            }
            xSemaphoreTake(s->guard, portMAX_DELAY);
            s->closing = false;
            s->session = 0;
            xSemaphoreGive(s->guard);
        }
    }
}
static void outgoing_task(void *arg) {
    int i = (intptr_t)arg;
    connection *s = &slots[i];
    uint8_t buf[U2_DATA_CHUNK];
    for (;;) {
        int idle_timeout = u2_config_int("tcp.idle_timeout_ms");
        xSemaphoreTake(s->tx_guard, portMAX_DELAY);
        xSemaphoreTake(s->guard, portMAX_DELAY);
        int fd = s->fd;
        uint32_t session = s->session, generation = s->generation;
        bool invalidated = session && !s->closing &&
                           !u2_network_stream_valid(i + 1, session);
        int got = fd >= 0 && !invalidated ? recv(fd, buf, sizeof(buf), MSG_DONTWAIT) : -1;
        int err = errno;
        if (got > 0) {
            s->read_bytes += got;
            s->activity = esp_timer_get_time();
        }
        bool ended = invalidated ||
                     (fd >= 0 && (got == 0 || (got < 0 && err != EAGAIN && err != EWOULDBLOCK)));
        if (fd >= 0 && esp_timer_get_time() - s->activity > (int64_t)idle_timeout * 1000) {
            s->last_error = ETIMEDOUT;
            ended = true;
        } else if (invalidated)
            s->last_error = is_cancelled(s, session) ? ECANCELED : ENETDOWN;
        else if (ended && got < 0)
            s->last_error = err;
        xSemaphoreGive(s->guard);
        if (got > 0)
            u2_send(i + 1, U2_DATA, buf, got, session);
        xSemaphoreGive(s->tx_guard);
        if (ended)
            close_generation(i, true, generation);
        if (got <= 0)
            vTaskDelay(pdMS_TO_TICKS(10));
    }
}
cJSON *u2_network_state(void) {
    cJSON *o = cJSON_CreateObject();
    cJSON_AddBoolToObject(o, "connected", connected);
    cJSON_AddNumberToObject(o, "disconnect_reason", disconnect_reason);
    cJSON_AddNumberToObject(o, "reconnects", reconnects);
    wifi_ap_record_t ap;
    if (connected && esp_wifi_sta_get_ap_info(&ap) == ESP_OK) {
        cJSON_AddNumberToObject(o, "rssi_dbm", ap.rssi);
        cJSON_AddNumberToObject(o, "channel", ap.primary);
        cJSON_AddStringToObject(o, "ssid", (char *)ap.ssid);
    } else
        cJSON_AddNullToObject(o, "rssi_dbm");
    int8_t tx_power;
    if (esp_wifi_get_max_tx_power(&tx_power) == ESP_OK)
        cJSON_AddNumberToObject(o, "max_tx_power_dbm", tx_power * 0.25);
    wifi_ps_type_t ps;
    if (esp_wifi_get_ps(&ps) == ESP_OK)
        cJSON_AddNumberToObject(o, "power_save", ps);
    cJSON_AddNumberToObject(o, "power_save_error", power_save_error);
    esp_netif_ip_info_t ip;
    char str[32];
    if (esp_netif_get_ip_info(sta, &ip) == ESP_OK) {
        snprintf(str, sizeof(str), IPSTR, IP2STR(&ip.ip));
        cJSON_AddStringToObject(o, "address", str);
        snprintf(str, sizeof(str), IPSTR, IP2STR(&ip.gw));
        cJSON_AddStringToObject(o, "gateway", str);
        snprintf(str, sizeof(str), IPSTR, IP2STR(&ip.netmask));
        cJSON_AddStringToObject(o, "netmask", str);
    }
    esp_netif_dns_info_t dns;
    if (esp_netif_get_dns_info(sta, ESP_NETIF_DNS_MAIN, &dns) == ESP_OK) {
        snprintf(str, sizeof(str), IPSTR, IP2STR(&dns.ip.u_addr.ip4));
        cJSON_AddStringToObject(o, "dns", str);
    }
    cJSON *a = cJSON_AddArrayToObject(o, "connections");
    for (int i = 0; i < 4; i++) {
        connection *s = &slots[i];
        cJSON *v = cJSON_CreateObject();
        xSemaphoreTake(s->guard, portMAX_DELAY);
        cJSON_AddNumberToObject(v, "channel", i + 1);
        cJSON_AddBoolToObject(v, "open", s->fd >= 0);
        cJSON_AddStringToObject(v, "phase",
                                s->closing   ? "closing"
                                : s->fd >= 0 ? "connected"
                                : s->session ? "connecting"
                                             : "closed");
        cJSON_AddStringToObject(v, "target", s->target);
        cJSON_AddNumberToObject(v, "rx_bytes", s->read_bytes);
        cJSON_AddNumberToObject(v, "tx_bytes", s->written_bytes);
        cJSON_AddNumberToObject(v, "last_error", s->last_error);
        cJSON_AddBoolToObject(v, "tcp_nodelay", s->fd >= 0);
        xSemaphoreGive(s->guard);
        cJSON_AddItemToArray(a, v);
    }
    return o;
}
void u2_network_init(void) {
    for (int i = 0; i < 4; i++) {
        slots[i].fd = -1;
        slots[i].guard = xSemaphoreCreateMutex();
        slots[i].tx_guard = xSemaphoreCreateMutex();
        slots[i].dial_queue = xQueueCreate(1, sizeof(dial_job));
        configASSERT(slots[i].guard && slots[i].tx_guard && slots[i].dial_queue);
        configASSERT(xTaskCreate(incoming_task, "tcp-write", 6144, (void *)(intptr_t)i, 4, NULL) ==
                     pdPASS);
        configASSERT(xTaskCreate(outgoing_task, "tcp-read", 6144, (void *)(intptr_t)i, 4, NULL) ==
                     pdPASS);
        configASSERT(xTaskCreate(dial_task, "tcp-connect", 6144, (void *)(intptr_t)i, 4, NULL) ==
                     pdPASS);
    }
    sta = esp_netif_create_default_wifi_sta();
    configASSERT(sta);
    wifi_init_config_t cfg = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&cfg));
    ESP_ERROR_CHECK(esp_wifi_set_storage(WIFI_STORAGE_RAM));
    ESP_ERROR_CHECK(esp_event_handler_register(WIFI_EVENT, ESP_EVENT_ANY_ID, wifi_event, NULL));
    ESP_ERROR_CHECK(esp_event_handler_register(IP_EVENT, IP_EVENT_STA_GOT_IP, wifi_event, NULL));
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_start());
    // This USB-powered gateway favors interactive latency over modem sleep.
    power_save_error = esp_wifi_set_ps(WIFI_PS_NONE);
    if (power_save_error != ESP_OK)
        u2_log("wifi.power_save_error", "Unable to disable Wi-Fi modem sleep");
}
cJSON *u2_wifi_scan(void) {
    wifi_scan_config_t config = {.show_hidden = true};
    cJSON *result = cJSON_CreateObject();
    esp_err_t err = esp_wifi_scan_start(&config, true);
    if (err != ESP_OK) {
        cJSON_AddStringToObject(result, "error", esp_err_to_name(err));
        return result;
    }
    wifi_ap_record_t *records = calloc(16, sizeof(*records));
    if (!records) {
        cJSON_Delete(result);
        return NULL;
    }
    uint16_t count = 16;
    esp_wifi_scan_get_ap_records(&count, records);
    cJSON *a = cJSON_AddArrayToObject(result, "networks");
    for (int i = 0; i < count; i++) {
        cJSON *v = cJSON_CreateObject();
        cJSON_AddStringToObject(v, "ssid", (char *)records[i].ssid);
        cJSON_AddNumberToObject(v, "rssi_dbm", records[i].rssi);
        cJSON_AddNumberToObject(v, "channel", records[i].primary);
        cJSON_AddNumberToObject(v, "authmode", records[i].authmode);
        cJSON_AddItemToArray(a, v);
    }
    free(records);
    return result;
}
