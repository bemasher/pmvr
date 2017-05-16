package main

import (
	"bufio"
	"bytes"
	"container/list"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"sync"
	"time"
)

const (
	// Default video resolution.
	vw = 1280
	vh = 720

	// MPEG-TS packets are all exactly 188 bytes.
	packetLength = 188
	// Named pipe raspivid will write motion vectors into and we'll read from.
	motionVectorPipe = "motion_vectors.fifo"

	// Logarithmic motion threshold for each frame.
	motionThreshold = 3.5
	// Number of consecutive frames that exceed the motion threshold before we
	// trigger recording. Takes care of noise and exposure/whitebalance adjustments.
	consecutiveMotion = 3

	// Number of seconds before and after motion to record.
	preRecord  = 5 * time.Second
	postRecord = 5 * time.Second

	// Some formatting strings for filenames and timestamp subtitles.
	srtTimeFmt     = "15:04:05.000"
	srtDateTimeFmt = "2006-01-02T15:04:05.0"
	fileTimeFmt    = "2006-01-02T15.04.05.000"
)

// A motion frame is an array of motion vectors.
type MotionFrame []byte

// Reterns a new motion frame for the given video width and height.
func NewMotionFrame(w, h int) MotionFrame {
	// MotionFrames are made up of macroblocks which are 16x16 pixels each.
	mw := (vw+15)>>4 + 1 // Always one extra column.
	mh := (vh + 15) >> 4

	return make([]byte, (mw*mh)<<2)
}

// Determine the magnitude of the motion for all of the vectors.
func (mv MotionFrame) Mag() (mag float64) {
	mag = 1.0
	for idx := 0; idx < len(mv); idx += 4 {
		abs := int(int8(mv[idx])) * int(int8(mv[idx]))
		abs += int(int8(mv[idx+1])) * int(int8(mv[idx+1]))
		mag += float64(abs)
	}
	return math.Log10(mag)
}

// A goroutine that consumes motion vector data and emits motion events on a channel.
func ConsumeMotion(motionVector io.Reader, motion, done chan struct{}) {
	defer func() {
		// If we exit because raspivid died and is no longer producing data, say so.
		done <- struct{}{}
	}()

	var (
		atEOF   bool
		counter int
		frame   = NewMotionFrame(vw, vh)
	)

	// Until we hit EOF
	for !atEOF {
		// Read a MotionFrame
		_, err := motionVector.Read(frame)

		atEOF = err == io.EOF
		if err != nil && !atEOF {
			return
		}

		// If the magnitude is above the threshold.
		if mag := frame.Mag(); mag > motionThreshold {
			// If the counter is above the consecutive MotionFrame threshold
			// and we're ready to record, then emit a motion signal.
			if counter > consecutiveMotion {
				log.Println("Motion Detected!")
				motion <- struct{}{}
				counter = 0
			}
			// Increment the frame counter.
			counter++
		} else {
			counter = 0
		}
	}
}

// Packets consist of a timestamp and an mpeg-ts packet.
type Packet struct {
	TimeStamp time.Time
	Payload   []byte
}

// A PacketQueue consists of a doubly-linked list and a lock.
type PacketQueue struct {
	*list.List

	// We don't use a sync.Mutex here because sometimes we'd like to attempt to
	// obtain the lock and continue execution if we're unable to obtain it.
	lock chan struct{}
}

func NewPacketQueue() PacketQueue {
	return PacketQueue{
		new(list.List),
		make(chan struct{}, 1),
	}
}

var startCode = []byte{0x00, 0x00, 0x00, 0x01}

func SplitFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	idx := bytes.Index(data[1:], startCode)
	if idx == -1 {
		return 0, nil, nil
	}

	return idx + 1, data[:idx+1], nil
}

// Consumes mpeg-ts packets from ffmpeg, stores them in a queue and removes old packets from the queue.
func ConsumeFFmpeg(r io.Reader, pktQueue *PacketQueue, record, done chan struct{}) {
	defer func() {
		// If we exit because ffmpeg died and is no longer producing data, say so.
		done <- struct{}{}
	}()

	// Use a pool to re-use packets.
	pktPool := &sync.Pool{
		New: func() interface{} {
			return Packet{}
		},
	}

	// Maintain a local queue.
	local := list.New()
	var lastNal []byte

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	scanner.Split(SplitFrames)

	for {
		select {
		// If we're able to obtain the packet queue lock, append our local list
		// to the main queue, reset it and release the lock.
		case pktQueue.lock <- struct{}{}:
			if local.Len() > 1 {
				pktQueue.PushBackList(local)
				local = list.New()
			}
			<-pktQueue.lock
		default:
		}

		// Get a new packet.
		pkt := pktPool.Get().(Packet)
		pkt.Payload = pkt.Payload[:0]

		// Set the timestamp
		pkt.TimeStamp = time.Now()

		if len(lastNal) != 0 {
			pkt.Payload = append(pkt.Payload, lastNal...)
		}

		// Read the packet.
		for scanner.Scan() {
			nal := scanner.Bytes()
			log.Printf("NAL: %d % 10d % 10d\n", nal[4]&0x1F, len(pkt.Payload), cap(pkt.Payload))

			if nal[4]&0x1F == 7 && len(pkt.Payload) > 0 {
				lastNal = nal
				break
			}

			pkt.Payload = append(pkt.Payload, nal...)
		}

		// Put it in the local queue.
		local.PushBack(pkt)

		select {
		// If we're able to obtain the packet queue lock.
		case pktQueue.lock <- struct{}{}:
			// If we're not currently recording, obtain the record lock.
			select {
			case record <- struct{}{}:
				var next *list.Element

				// Determine how old packets are allowed to be.
				cutoff := time.Now().Add(-preRecord)
				// Prune expired packets from the front of the queue.
				for e := pktQueue.Front(); e != nil; e = next {
					if pkt, ok := e.Value.(Packet); ok && pkt.TimeStamp.Before(cutoff) {
						// pktQueue.Remove(e) clears value of Next(), so store it first.
						next = e.Next()
						// Remove the element from the queue.
						pktQueue.Remove(e)
						// Put the packet back in the pool.
						pktPool.Put(pkt)
					} else {
						// Packets should always be in order in the queue, so
						// stop if we've encountered one too young to remove.
						break
					}
				}
				// Release the record lock.
				<-record
			default:
			}
			// Release the queue lock.
			<-pktQueue.lock
		default:
		}
	}
}

func init() {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
}

func main() {
	pprofFile, err := os.Create("pprof.cpu")
	if err != nil {
		log.Fatal(err)
	}
	defer pprofFile.Close()

	pprof.StartCPUProfile(pprofFile)
	defer pprof.StopCPUProfile()

	// Setup signal notification, so we can exit gracefully.
	sig := make(chan os.Signal)
	signal.Notify(sig, os.Interrupt, os.Kill)

	// Specification of raspivid command flags.
	raspivid := exec.Command(
		"raspivid", "-w", strconv.Itoa(vw), "-h", strconv.Itoa(vh), "-fps", "30", "-t", "0",
		"-n", "-g", "10", "-ih", "-x", motionVectorPipe, "-o", "-",
	)

	raspividStdout, _ := raspivid.StdoutPipe()
	defer raspividStdout.Close()

	log.Println("Starting: raspivid")
	if err := raspivid.Start(); err != nil {
		log.Fatal(err)
	}

	// Open the named pipe for motion vectors.
	log.Printf("Open %s\n", motionVectorPipe)
	motionVector, err := os.OpenFile(motionVectorPipe, os.O_RDONLY, os.ModeNamedPipe)
	if err != nil {
		log.Fatal(err)
	}
	defer motionVector.Close()

	// Make a new packet queue, this is the primary queue.
	pktQueue := NewPacketQueue()

	// Setup "maybe" locks for motion events and recording.
	motion := make(chan struct{}, 1)
	record := make(chan struct{}, 1)

	// Setup done signal for the event that either of our consumers die.
	done := make(chan struct{})

	// Setup a timer for post-record length.
	after := time.NewTimer(0)
	after.Stop()

	// Run both of our consumers.
	go ConsumeMotion(motionVector, motion, done)
	go ConsumeFFmpeg(raspividStdout, &pktQueue, record, done)

	for {
		select {
		case s := <-sig:
			// If we've received a signal, exit.
			log.Println("Terminating: ", s)
			return
		case <-done:
			// If either of our consumers have died, exit.
			log.Println("Terminating")
			return
		case <-motion:
			// We've received a motion event.

			// Attempt to obtain the record lock. It may already be locked.
			select {
			case record <- struct{}{}:
			default:
			}
			// Reset the post recording timer.
			after.Reset(postRecord)
		case <-after.C:
			// The post recording timer expired.
			after.Stop()

			// Obtain a lock on the packet queue.
			pktQueue.lock <- struct{}{}

			// Get the packet at the front of the queue, it is the oldest packet.
			if pkt, ok := pktQueue.Front().Value.(Packet); ok {
				// Determine our filename base.
				timeStamp := pkt.TimeStamp.Format(fileTimeFmt)

				// Create the video file.
				log.Println("Creating:", timeStamp+".h264")
				videoFile, err := os.Create(timeStamp + ".h264")
				if err != nil {
					log.Fatal(err)
				}

				// For each packet in the queue.
				for e := pktQueue.Front(); e != nil; e = e.Next() {
					if pkt, ok := e.Value.(Packet); ok {
						// Write the packet's payload to the video file.
						_, err := videoFile.Write(pkt.Payload)
						if err != nil {
							log.Println(err)
						}
					}
				}
				videoFile.Close()
			}
			// Release the packet queue lock.
			<-pktQueue.lock

			// Release the recording lock so we can resume discarding old packets.
			<-record
		}
	}
}
