// vmoto-crop: sets the crop window of the SigmaStar scaler (VPE channel 0, output port 0) while majestic runs.
// The window is in sensor coordinates and is scaled to the video size -> digital zoom with the sensor's full detail.
//   vmoto-crop X Y W H
// Needs libmi_sys.so and libmi_vpe.so (OpenIPC package sigmastar-osdrv-infinity6e), e.g. LD_LIBRARY_PATH=/etc/vmoto/lib.
#include <stdio.h>
#include <stdlib.h>

typedef struct { unsigned short x, y, w, h; } Rect; // MI_SYS_WindowRect_t

extern int MI_SYS_Init(void);
extern int MI_VPE_SetPortCrop(int ch, int port, Rect *r);
extern int MI_VPE_GetPortCrop(int ch, int port, Rect *r);

int main(int argc, char **argv) {
	if (argc != 5) {
		fprintf(stderr, "usage: %s X Y W H\n", argv[0]);
		return 2;
	}
	Rect r = {(unsigned short)atoi(argv[1]), (unsigned short)atoi(argv[2]),
	          (unsigned short)atoi(argv[3]), (unsigned short)atoi(argv[4])};
	int ret = MI_SYS_Init();
	if (ret) {
		fprintf(stderr, "MI_SYS_Init: 0x%x\n", ret);
		return 1;
	}
	ret = MI_VPE_SetPortCrop(0, 0, &r);
	Rect g = {0, 0, 0, 0};
	MI_VPE_GetPortCrop(0, 0, &g);
	printf("crop %u,%u %ux%u ret=0x%x\n", g.x, g.y, g.w, g.h, ret);
	return ret != 0;
}
