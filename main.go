package main

import (
	"bufio"
	"bytes"
	"container/list"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Default video resolution.
	vw = 1280
	vh = 720

	// Named pipe raspivid will write motion vectors into and we'll read from.
	motionVectorPipe = "motion_vectors.fifo"

	// Vectors with magnitude lower than this threshold are ignored.
	magnitudeThreshold = 4

	// Number of consecutive frames that exceed the motion threshold before we
	// trigger recording. This is primarily used to avoid triggers due to noise
	// or exposure changes.
	temporalThreshold = 1

	// Minimum number of vectors above threshold.
	vectorCountThreshold = 1

	// Threshold below which the SAD for a vector must be in order to be considered reliable.
	sadThreshold = 1024

	// Number of seconds before and after motion to record.
	preRecord  = 2500 * time.Millisecond
	postRecord = 2500 * time.Millisecond

	// Some formatting strings for filenames and timestamp subtitles.
	srtTimeFmt     = "15:04:05.000"
	srtDateTimeFmt = "2006-01-02T15:04:05.0"
	fileTimeFmt    = "2006-01-02T15.04.05.000"
)

// A motion frame is an array of motion vectors.
type MotionFrame []byte

// Returns a new motion frame for the given video width and height.
func NewMotionFrame(w, h int) MotionFrame {
	// MotionFrames are made up of macroblocks which are 16x16 pixels each.
	mw := (vw+15)>>4 + 1 // Always one extra column.
	mh := (vh + 15) >> 4

	return make([]byte, (mw*mh)<<2)
}

// Determine the magnitude of the motion for all of the vectors.
func (mv MotionFrame) AboveThreshold() bool {
	var (
		min   int
		count int
	)

	maxSad := uint16(0) // Smallest uint16 is 0
	minSad := ^maxSad   // Largest uint16 is 65535

	for idx := 0; idx < len(mv); idx += 4 {
		abs := int(int8(mv[idx])) * int(int8(mv[idx]))
		abs += int(int8(mv[idx+1])) * int(int8(mv[idx+1]))

		if abs > min {
			min = abs
		}

		sad := uint16(mv[idx+2]) | uint16(mv[idx+3])<<8
		if sad < sadThreshold && abs > magnitudeThreshold {
			count++

			if sad < minSad {
				minSad = sad
			}
			if sad > maxSad {
				maxSad = sad
			}
		}
	}

	if count > vectorCountThreshold {
		log.Printf("%6d %6d %6d %6d\n", min, count, minSad, maxSad)
	}

	return count > vectorCountThreshold
}

// A goroutine that consumes motion vector data and emits motion events on a channel.
func ConsumeMotion(motionVector io.Reader, motion, done chan struct{}) {
	defer func() {
		// If we exit because raspivid died and is no longer producing data, say so.
		done <- struct{}{}
	}()

	var (
		atEOF      bool
		counter    int
		frame      = NewMotionFrame(vw, vh)
		startDelay = true
	)

	time.AfterFunc(time.Second*5, func() {
		startDelay = false
	})

	// Until we hit EOF
	for !atEOF {
		// Read a MotionFrame
		_, err := motionVector.Read(frame)

		atEOF = err == io.EOF
		if err != nil && !atEOF {
			return
		}

		if startDelay {
			continue
		}

		// If the magnitude is above the threshold.
		if frame.AboveThreshold() {
			// If the counter is above the consecutive MotionFrame threshold
			// and we're ready to record, then emit a motion signal.
			if counter > temporalThreshold {
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

// Consumes h.264 packets from raspivid, stores them in a queue and removes old packets from the queue.
func ConsumeVideo(r io.Reader, pktQueue *PacketQueue, record, done chan struct{}) {
	defer func() {
		// If we exit because raspivid died and is no longer producing data, say so.
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

func RemuxH264(filename string) {
	log.Println("Remuxing:", filename)

	ext := filepath.Ext(filename)
	newFilename := strings.TrimSuffix(filename, ext) + ".mp4"

	mp4box := exec.Command("MP4Box", "-add", filename, "-fps", "30.0", "-new", newFilename)
	mp4box.Stdout = os.Stdout
	mp4box.Stderr = os.Stderr
	err := mp4box.Run()
	if err != nil {
		log.Println(err)
		return
	}

	os.Remove(filename)
}

func init() {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
}

func main() {
	// Setup signal notification, so we can exit gracefully.
	sig := make(chan os.Signal)
	signal.Notify(sig, os.Interrupt, os.Kill)

	// Specification of raspivid command flags.
	raspivid := exec.Command(
		"raspivid", "-w", strconv.Itoa(vw), "-h", strconv.Itoa(vh), "-fps", "30", "-t", "0",
		"-n", "-g", "10", "-vf", "-ih", "-x", motionVectorPipe, "-o", "-",
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
	go ConsumeVideo(raspividStdout, &pktQueue, record, done)

	for {
		select {
		case s := <-sig:
			// If we've received a signal, exit.
			log.Println("Terminating: ", s)
			return
		case <-done:
			// If either of our consumers have died, exit.
			log.Println("Terminating: consumer died")
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
				filename := timeStamp + ".h264"

				// Create the video file.
				log.Println("Creating:", filename)
				videoFile, err := os.Create(filename)
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
				go RemuxH264(filename)
			}
			// Release the packet queue lock.
			<-pktQueue.lock

			// Release the recording lock so we can resume discarding old packets.
			<-record
		}
	}
}
