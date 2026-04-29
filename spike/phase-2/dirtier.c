/* dirtier — write to a memory region at a controlled rate.
 *
 * Usage: dirtier <bytes_total> <bytes_per_iter> <sleep_us_between_iters>
 *
 * Allocates `bytes_total`, writes `bytes_per_iter` distinct pages (one
 * byte per page) per iteration, sleeps `sleep_us_between_iters`. Reports
 * pages-per-sec to stdout every 5 seconds.
 *
 * Single-threaded. Pages dirtied at random offsets so each iteration
 * dirties a different set.
 *
 * Static-linked for the spike guest (BusyBox musl rootfs without libc).
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <time.h>
#include <sys/mman.h>
#include <stdint.h>

#define PAGE_SIZE 4096

static uint64_t now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

int main(int argc, char **argv) {
    if (argc != 4) {
        fprintf(stderr, "usage: %s <bytes_total> <bytes_per_iter> <sleep_us>\n", argv[0]);
        return 1;
    }
    uint64_t total = strtoull(argv[1], NULL, 10);
    uint64_t per_iter = strtoull(argv[2], NULL, 10);
    uint64_t sleep_us = strtoull(argv[3], NULL, 10);

    uint64_t pages_total = total / PAGE_SIZE;
    uint64_t pages_per_iter = per_iter / PAGE_SIZE;
    if (pages_per_iter == 0) pages_per_iter = 1;

    fprintf(stderr, "DIRTIER total=%llu MiB pages=%llu pages/iter=%llu sleep=%llu us\n",
            (unsigned long long)(total / 1048576),
            (unsigned long long)pages_total,
            (unsigned long long)pages_per_iter,
            (unsigned long long)sleep_us);

    /* mmap anonymous + populate (force allocation up front) */
    void *region = mmap(NULL, total, PROT_READ | PROT_WRITE,
                        MAP_ANONYMOUS | MAP_PRIVATE | MAP_POPULATE, -1, 0);
    if (region == MAP_FAILED) {
        perror("mmap");
        return 2;
    }
    /* Touch every page once to ensure it's mapped */
    for (uint64_t i = 0; i < pages_total; i++) {
        ((volatile char *)region)[i * PAGE_SIZE] = (char)i;
    }
    fprintf(stderr, "DIRTIER mapped and populated %llu MiB\n", (unsigned long long)(total / 1048576));

    /* Tight loop: dirty pages_per_iter random pages, sleep sleep_us */
    uint64_t iter = 0;
    uint64_t pages_dirtied = 0;
    uint64_t window_start = now_ns();
    uint64_t window_pages = 0;

    /* Use a simple LCG so we don't need rand_r(); fast and deterministic. */
    uint64_t seed = 0x9E3779B97F4A7C15ULL;

    for (;;) {
        for (uint64_t k = 0; k < pages_per_iter; k++) {
            seed = seed * 6364136223846793005ULL + 1442695040888963407ULL;
            uint64_t off = (seed % pages_total) * PAGE_SIZE;
            ((volatile char *)region)[off] = (char)(iter ^ k);
        }
        pages_dirtied += pages_per_iter;
        window_pages += pages_per_iter;
        iter++;
        if (sleep_us > 0) usleep(sleep_us);

        uint64_t now = now_ns();
        if (now - window_start >= 5ULL * 1000000000ULL) {
            double secs = (double)(now - window_start) / 1e9;
            double pages_per_sec = (double)window_pages / secs;
            double mib_per_sec = pages_per_sec * PAGE_SIZE / 1048576.0;
            printf("DIRTYRATE iter=%llu pages=%llu pages/s=%.1f MiB/s=%.1f\n",
                   (unsigned long long)iter,
                   (unsigned long long)pages_dirtied,
                   pages_per_sec, mib_per_sec);
            fflush(stdout);
            window_start = now;
            window_pages = 0;
        }
    }
    return 0;
}
