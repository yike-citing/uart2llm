#include "u2.h"
#include "nvs_flash.h"
#include "esp_event.h"
#include "esp_netif.h"
#include "esp_psram.h"
#include "esp_ota_ops.h"
#include "esp_log.h"
#include "esp_flash.h"
#include "esp_heap_caps.h"

void *u2_buffer_alloc(size_t length) {
    return heap_caps_malloc(length, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
}

void app_main(void) {
    // Never erase NVS automatically: damaged or incompatible storage needs operator recovery.
    ESP_ERROR_CHECK(nvs_flash_init());
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    if (!esp_psram_is_initialized() || esp_psram_get_size() < 8 * 1024 * 1024) {
        ESP_LOGE("uart2llm", "8 MiB PSRAM required; refusing to start");
        return;
    }
    uint32_t flash_size = 0;
    if (esp_flash_get_size(NULL, &flash_size) != ESP_OK || flash_size < 16 * 1024 * 1024) {
        ESP_LOGE("uart2llm", "16 MiB Flash required; refusing to start");
        return;
    }
    u2_network_init();
    u2_management_init();
    u2_link_init();
    configASSERT(xTaskCreate(u2_management_task, "management", 16384, NULL, 6, NULL) == pdPASS);
    // Candidate only becomes valid after subsystem allocation and authenticated host self-test.
    for (;;) {
        u2_maintenance_tick();
        vTaskDelay(pdMS_TO_TICKS(100));
    }
}
