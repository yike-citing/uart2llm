idf_component_register(SRCS "main.c" "link.c" "network.c" "management.c"
    INCLUDE_DIRS "."
    REQUIRES esp_driver_uart esp_driver_usb_serial_jtag esp_wifi esp_netif nvs_flash app_update protocomm esp_timer esp_psram esp_system mbedtls esp_driver_gpio espcoredump spi_flash)
