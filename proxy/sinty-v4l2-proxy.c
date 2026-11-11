#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/videodev2.h>
#include <poll.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>

enum {
	OP_OPEN = 1,
	OP_IOCTL = 2,
	OP_MMAP = 3,
	OP_WRITE = 4,
	OP_READ = 5,
	OP_POLL = 6,
	OP_CLOSE = 7,
	OP_UNMAP = 8,
};

#define MAX_MAPS 256
#define MAX_ARG 4096
#define MAX_PLANES VIDEO_MAX_PLANES

static int (*v_open)(const char *, int, ...);
static int (*v_close)(int);
static int (*v_ioctl)(int, unsigned long, ...);
static void *(*v_mmap)(void *, size_t, int, int, int, int64_t);
static int (*v_munmap)(void *, size_t);

struct map {
	uint32_t offset;
	uint32_t length;
	uint8_t *mem;
};

struct stash {
	int full;
	struct v4l2_buffer buf;
	struct v4l2_plane planes[MAX_PLANES];
};

static struct map maps[MAX_MAPS];
static int fd = -1;
static uint32_t cap_type, out_type;
static uint32_t nplanes[16];
static struct stash stashes[16];
static int event_full;
static struct v4l2_event event;

static int sys_open(const char *path, int flags, ...)
{
	return open(path, flags);
}

static int sys_ioctl(int f, unsigned long req, ...)
{
	va_list ap;
	va_start(ap, req);
	void *arg = va_arg(ap, void *);
	va_end(ap);
	return ioctl(f, req, arg);
}

static void *sys_mmap(void *addr, size_t len, int prot, int flags, int f, int64_t off)
{
	return mmap(addr, len, prot, flags, f, off);
}

#define TEGRA_LIB "/usr/lib/aarch64-linux-gnu/nvidia/"

static void bind_library(void)
{
	const char *names[] = { getenv("SINTY_V4L2_LIBRARY"), TEGRA_LIB "libnvv4l2.so", "libv4l2.so.0" };
	if (access(TEGRA_LIB "libnvv4l2.so", R_OK) == 0)
		dlopen(TEGRA_LIB "libnvv4lconvert.so", RTLD_NOW | RTLD_GLOBAL);
	for (size_t i = 0; i < sizeof names / sizeof names[0]; i++) {
		if (!names[i] || !*names[i])
			continue;
		void *h = dlopen(names[i], RTLD_NOW);
		if (!h)
			continue;
		v_open = dlsym(h, "v4l2_open");
		v_close = dlsym(h, "v4l2_close");
		v_ioctl = dlsym(h, "v4l2_ioctl");
		v_mmap = dlsym(h, "v4l2_mmap");
		v_munmap = dlsym(h, "v4l2_munmap");
		if (v_open && v_close && v_ioctl && v_mmap && v_munmap)
			return;
	}
	v_open = sys_open;
	v_close = close;
	v_ioctl = sys_ioctl;
	v_mmap = sys_mmap;
	v_munmap = munmap;
}

static int read_full(void *p, size_t n)
{
	uint8_t *b = p;
	while (n) {
		ssize_t r = read(0, b, n);
		if (r <= 0) {
			if (r < 0 && errno == EINTR)
				continue;
			return -1;
		}
		b += r;
		n -= r;
	}
	return 0;
}

static int write_full(const void *p, size_t n)
{
	const uint8_t *b = p;
	while (n) {
		ssize_t r = write(1, b, n);
		if (r <= 0) {
			if (r < 0 && errno == EINTR)
				continue;
			return -1;
		}
		b += r;
		n -= r;
	}
	return 0;
}

static uint32_t get32(void)
{
	uint32_t v = 0;
	if (read_full(&v, 4))
		exit(0);
	return v;
}

static void put32(uint32_t v)
{
	if (write_full(&v, 4))
		exit(0);
}

static struct map *find_map(uint32_t offset)
{
	for (int i = 0; i < MAX_MAPS; i++)
		if (maps[i].mem && maps[i].offset == offset)
			return &maps[i];
	return NULL;
}

static void drop_map(uint32_t offset)
{
	struct map *m = find_map(offset);
	if (m) {
		v_munmap(m->mem, m->length);
		m->mem = NULL;
	}
}

static int is_mplane(uint32_t type)
{
	return type == V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE || type == V4L2_BUF_TYPE_VIDEO_OUTPUT_MPLANE;
}

static int is_buffer_ioctl(unsigned long req)
{
	return req == VIDIOC_QBUF || req == VIDIOC_DQBUF || req == VIDIOC_QUERYBUF || req == VIDIOC_PREPARE_BUF;
}

static int try_dequeue(uint32_t type)
{
	struct stash *s = &stashes[type & 15];
	if (s->full)
		return 1;
	memset(&s->buf, 0, sizeof s->buf);
	s->buf.type = type;
	s->buf.memory = V4L2_MEMORY_MMAP;
	if (is_mplane(type)) {
		memset(s->planes, 0, sizeof s->planes);
		s->buf.m.planes = s->planes;
		s->buf.length = nplanes[type & 15] ? nplanes[type & 15] : 1;
	}
	if (v_ioctl(fd, VIDIOC_DQBUF, &s->buf) == 0) {
		s->full = 1;
		return 1;
	}
	return 0;
}

static int try_event(void)
{
	if (event_full)
		return 1;
	memset(&event, 0, sizeof event);
	if (v_ioctl(fd, VIDIOC_DQEVENT, &event) == 0) {
		event_full = 1;
		return 1;
	}
	return 0;
}

static int64_t now_ms(void)
{
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

static uint32_t emulated_poll(int16_t events, int32_t ms)
{
	int64_t deadline = now_ms() + (ms < 0 ? 0 : ms);
	for (;;) {
		uint32_t rev = 0;
		if ((events & POLLIN) && cap_type && try_dequeue(cap_type))
			rev |= POLLIN;
		if ((events & POLLOUT) && out_type && try_dequeue(out_type))
			rev |= POLLOUT;
		if ((events & POLLPRI) && try_event())
			rev |= POLLPRI;
		if (rev || now_ms() >= deadline)
			return rev;
		struct timespec pause = { 0, 1000000 };
		nanosleep(&pause, NULL);
	}
}

static int32_t do_ioctl(unsigned long req, uint8_t *arg, uint32_t alen, uint8_t *planes, uint32_t plen)
{
	if (req == VIDIOC_DQEVENT && event_full && alen >= sizeof event) {
		memcpy(arg, &event, sizeof event);
		event_full = 0;
		return 0;
	}
	if (is_buffer_ioctl(req) && alen >= sizeof(struct v4l2_buffer)) {
		struct v4l2_buffer *b = (struct v4l2_buffer *)arg;
		uint32_t t = b->type & 15;
		if (is_mplane(b->type)) {
			b->m.planes = (struct v4l2_plane *)planes;
			if (b->length * sizeof(struct v4l2_plane) > plen)
				b->length = plen / sizeof(struct v4l2_plane);
			if (req != VIDIOC_DQBUF && b->length)
				nplanes[t] = b->length;
		}
		if (req == VIDIOC_DQBUF && stashes[t].full) {
			struct stash *s = &stashes[t];
			uint32_t n = is_mplane(b->type) ? s->buf.length : 0;
			void *keep = b->m.planes;
			*b = s->buf;
			if (n) {
				memcpy(planes, s->planes, n * sizeof(struct v4l2_plane));
				b->m.planes = keep;
			}
			s->full = 0;
			return 0;
		}
	}
	if (req == VIDIOC_REQBUFS && alen >= sizeof(struct v4l2_requestbuffers)) {
		struct v4l2_requestbuffers *r = (struct v4l2_requestbuffers *)arg;
		if (V4L2_TYPE_IS_OUTPUT(r->type))
			out_type = r->type;
		else
			cap_type = r->type;
		stashes[r->type & 15].full = 0;
	}
	if (req == VIDIOC_STREAMOFF && alen >= 4) {
		uint32_t type;
		memcpy(&type, arg, 4);
		stashes[type & 15].full = 0;
	}
	if (v_ioctl(fd, req, arg) != 0)
		return errno ? errno : EIO;
	return 0;
}

static void serve(void)
{
	static uint8_t arg[MAX_ARG];
	static uint8_t planes[MAX_PLANES * sizeof(struct v4l2_plane)];
	for (;;) {
		uint32_t op = get32();
		switch (op) {
		case OP_OPEN: {
			uint32_t n = get32();
			char path[4096];
			if (n >= sizeof path || read_full(path, n))
				exit(1);
			path[n] = 0;
			fd = v_open(path, O_RDWR | O_NONBLOCK | O_CLOEXEC);
			put32(fd < 0 ? (uint32_t)errno : 0);
			break;
		}
		case OP_IOCTL: {
			uint64_t req;
			if (read_full(&req, 8))
				exit(0);
			uint32_t alen = get32();
			if (alen > sizeof arg || read_full(arg, alen))
				exit(1);
			uint32_t plen = get32();
			if (plen > sizeof planes || read_full(planes, plen))
				exit(1);
			int32_t err = do_ioctl((unsigned long)req, arg, alen, planes, plen);
			put32((uint32_t)err);
			if (write_full(arg, alen) || write_full(planes, plen))
				exit(0);
			break;
		}
		case OP_MMAP: {
			uint32_t offset = get32(), length = get32();
			drop_map(offset);
			int slot = -1;
			for (int i = 0; i < MAX_MAPS && slot < 0; i++)
				if (!maps[i].mem)
					slot = i;
			void *mem = slot < 0 ? MAP_FAILED : v_mmap(NULL, length, PROT_READ | PROT_WRITE, MAP_SHARED, fd, offset);
			if (mem == MAP_FAILED) {
				put32(slot < 0 ? ENOMEM : (uint32_t)errno);
				break;
			}
			maps[slot] = (struct map){ offset, length, mem };
			put32(0);
			break;
		}
		case OP_UNMAP:
			drop_map(get32());
			put32(0);
			break;
		case OP_WRITE: {
			uint32_t offset = get32(), n = get32();
			struct map *m = find_map(offset);
			uint8_t sink[4096];
			if (m && n <= m->length) {
				if (read_full(m->mem, n))
					exit(0);
				put32(0);
				break;
			}
			while (n) {
				uint32_t k = n < sizeof sink ? n : sizeof sink;
				if (read_full(sink, k))
					exit(0);
				n -= k;
			}
			put32(EINVAL);
			break;
		}
		case OP_READ: {
			uint32_t offset = get32(), n = get32();
			struct map *m = find_map(offset);
			if (!m || n > m->length) {
				put32(EINVAL);
				break;
			}
			put32(0);
			if (write_full(m->mem, n))
				exit(0);
			break;
		}
		case OP_POLL: {
			int32_t events = (int32_t)get32();
			int32_t ms = (int32_t)get32();
			put32(emulated_poll((int16_t)events, ms));
			break;
		}
		case OP_CLOSE:
			for (int i = 0; i < MAX_MAPS; i++)
				if (maps[i].mem)
					drop_map(maps[i].offset);
			if (fd >= 0)
				v_close(fd);
			put32(0);
			exit(0);
		default:
			exit(1);
		}
	}
}

int main(void)
{
	bind_library();
	serve();
	return 0;
}
