# PMVR
The Raspberry **P**i **M**otion **V**ideo **R**ecorder is a proof of concept daemon that makes use of the motion estimation vectors available from the RPi's hardware accelerated H.264 encoder to detect and save clips of motion.

# Setup
  * Requires the Go toolchain (>=go1.8)
    * I've not checked this for standard library compatibility with anything prior to `go1.8`. It may or may not work on versions prior to that.
  * Raspivid
    * On Arch Linux this is provided as part of `raspberrypi-firmware` but is not listed in the path. It must be in the path.
  * FFmpeg (or avconv symlinked as ffmpeg, if you must)
  * A named pipe for motion vectors: `mkfifo motion_vectors.fifo`

# Installation
```go
go get -v github.com/bemasher/pmvr
go install -v github.com/bemasher/pmvr
```

# Running
```bash
pmvr
```

# Improvements and Feature Requests
  * Have any improvements or feature requests? Submit an issue and we'll discuss feasibility.

# ToDo
- [ ] Add command line flags for:
  - [ ] Motion detection thresholds.
  - [ ] Raspivid command line options.
  - [ ] FFmpeg command line options.
