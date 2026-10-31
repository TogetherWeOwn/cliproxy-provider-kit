#include "abi.h"
#include <dlfcn.h>
#include <limits.h>
#include <stdio.h>
#include <string.h>

#define CHECK(condition) do { if (!(condition)) { \
    fprintf(stderr, "ABI assertion failed at line %d\n", __LINE__); \
    return 1; \
} } while (0)

typedef int (*init_fn)(cliproxy_host_api *, cliproxy_plugin_api *);
static int host_call(void *ctx, const char *method, const uint8_t *data,
                     size_t len, cliproxy_buffer *response) {
    (void)ctx; (void)method; (void)data; (void)len;
    const char result[] = "{\"ok\":true,\"result\":{}}";
    response->ptr = malloc(sizeof(result) - 1);
    if (!response->ptr) return 1;
    memcpy(response->ptr, result, sizeof(result) - 1);
    response->len = sizeof(result) - 1;
    return 0;
}
static void host_free(void *ptr, size_t len) { (void)len; free(ptr); }

static int contains(cliproxy_buffer response, const char *needle) {
    if (!response.ptr || !response.len) return 0;
    char *text = malloc(response.len + 1);
    if (!text) return 0;
    memcpy(text, response.ptr, response.len);
    text[response.len] = '\0';
    int found = strstr(text, needle) != NULL;
    free(text);
    return found;
}

int main(int argc, char **argv) {
    CHECK(argc == 2);
    void *image = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    CHECK(image != NULL);
    init_fn initialize = (init_fn)dlsym(image, "cliproxy_plugin_init");
    CHECK(initialize != NULL);
    cliproxy_host_api host = { 1, NULL, host_call, host_free };
    cliproxy_plugin_api plugin = { 0 };
    CHECK(initialize(NULL, &plugin) != 0);
    CHECK(initialize(&host, NULL) != 0);
    host.abi_version = 2;
    CHECK(initialize(&host, &plugin) != 0);
    host.abi_version = 1;
    host.free_buffer = NULL;
    CHECK(initialize(&host, &plugin) != 0);
    host.free_buffer = host_free;
    CHECK(initialize(&host, &plugin) == 0);
    CHECK(plugin.abi_version == 1 && plugin.call && plugin.free_buffer && plugin.shutdown);
    CHECK(initialize(&host, &plugin) != 0);

    cliproxy_buffer response = { NULL, 0 };
    CHECK(plugin.call(NULL, NULL, 0, &response) != 0);
    CHECK(contains(response, "invalid_request"));
    plugin.free_buffer(response.ptr, response.len);
    CHECK(plugin.call("plugin.register", NULL, 1, &response) != 0);
    CHECK(contains(response, "invalid_request"));
    plugin.free_buffer(response.ptr, response.len);
    CHECK(plugin.call("plugin.register", NULL, (size_t)INT_MAX + 1, &response) != 0);
    plugin.free_buffer(response.ptr, response.len);
    CHECK(plugin.call("plugin.register", NULL, 0, NULL) != 0);
    plugin.free_buffer(NULL, 0);

    /* enabled: true\nupstream_url: http://127.0.0.1:1\n -- no network call. */
    char registration[] = "{\"schema_version\":9,\"config_yaml\":\"ZW5hYmxlZDogdHJ1ZQp1cHN0cmVhbV91cmw6IGh0dHA6Ly8xMjcuMC4wLjE6MQo=\"}";
    CHECK(plugin.call("plugin.register", (uint8_t *)registration, strlen(registration), &response) == 0);
    CHECK(contains(response, "\"ok\":true") && contains(response, "\"schema_version\":9"));
    plugin.free_buffer(response.ptr, response.len);
    CHECK(plugin.call("plugin.quiesce", NULL, 0, &response) == 0);
    CHECK(contains(response, "\"ok\":true"));
    plugin.free_buffer(response.ptr, response.len);
    CHECK(plugin.call("executor.identifier", NULL, 0, &response) == 0);
    CHECK(contains(response, "not_ready"));
    plugin.free_buffer(response.ptr, response.len);
    plugin.shutdown();
    plugin.shutdown();
    CHECK(plugin.call("executor.identifier", NULL, 0, &response) != 0);
    CHECK(contains(response, "not_ready"));
    plugin.free_buffer(response.ptr, response.len);
    CHECK(initialize(&host, &plugin) == 0);
    plugin.shutdown();
    /* Deliberately no dlclose: the Go runtime stays resident until exit. */
    return 0;
}
