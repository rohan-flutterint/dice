package server

import (
	"context"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dicedb/dice/config"
	"github.com/dicedb/dice/core"
	"github.com/dicedb/dice/core/iomultiplexer"
)

var cronFrequency time.Duration = 1 * time.Second
var lastCronExecTime time.Time = time.Now()

const EngineStatus_WAITING int32 = 1 << 1
const EngineStatus_BUSY int32 = 1 << 2
const EngineStatus_SHUTTING_DOWN int32 = 1 << 3
const EngineStatus_TRANSACTION int32 = 1 << 4

var eStatus int32 = EngineStatus_WAITING

var connectedClients map[int]*core.Client

func init() {
	connectedClients = make(map[int]*core.Client)
}

// Waits on `core.WatchChannel` to receive updates about keys. Sends the update
// to all the clients that are watching the key.
// The message sent to the client will contain the new value and the operation
// that was performed on the key.
func WatchKeys(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case event := <-core.WatchChannel:
			core.WatchListMutex.Lock()
			if clients, ok := core.WatchList[event.Key]; ok {
				for clientFd := range clients {
					_, err := syscall.Write(clientFd, core.Encode(event, false))

					// if the client is not reachable, remove it from the watch list.
					if err != nil {
						delete(clients, clientFd)
					}
				}
			}
			core.WatchListMutex.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

func WaitForSignal(wg *sync.WaitGroup, sigs chan os.Signal) {
	defer wg.Done()
	<-sigs

	// if server is busy continue to wait
	for atomic.LoadInt32(&eStatus) == EngineStatus_BUSY {
	}

	// CRITICAL TO HANDLE
	// We do not want server to ever go back to BUSY state
	// when control flow is here ->

	// immediately set the status to be SHUTTING DOWN
	// the only place where we can set the status to be SHUTTING DOWN
	atomic.StoreInt32(&eStatus, EngineStatus_SHUTTING_DOWN)

	// if server is in any other state, initiate a shutdown
	core.Shutdown()
	os.Exit(0)
}

func RunAsyncTCPServer(wg *sync.WaitGroup) error {
	defer wg.Done()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer func() {
		atomic.StoreInt32(&eStatus, EngineStatus_SHUTTING_DOWN)
	}()

	log.Println("starting an asynchronous TCP server on", config.Host, config.Port)

	maxClients := 20000

	wg.Add(1)
	go WatchKeys(ctx, wg)

	// Create a socket
	serverFD, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(serverFD)

	// Set the Socket operate in a non-blocking mode
	if err = syscall.SetNonblock(serverFD, true); err != nil {
		return err
	}

	// Bind the IP and the port
	ip4 := net.ParseIP(config.Host)

	if err = syscall.Bind(serverFD, &syscall.SockaddrInet4{
		Port: config.Port,
		Addr: [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]},
	}); err != nil {
		return err
	}

	// Start listening
	if err = syscall.Listen(serverFD, maxClients); err != nil {
		return err
	}

	// AsyncIO starts here!!

	// creating multiplexer instance
	var multiplexer iomultiplexer.IOMultiplexer
	multiplexer, err = iomultiplexer.New(maxClients)
	if err != nil {
		log.Fatal(err)
	}
	defer multiplexer.Close()

	// Listen to read events on the Server itself
	if err := multiplexer.Subscribe(iomultiplexer.Event{
		Fd: serverFD,
		Op: iomultiplexer.OP_READ,
	}); err != nil {
		return err
	}

	// loop until the server is not shutting down
	for atomic.LoadInt32(&eStatus) != EngineStatus_SHUTTING_DOWN {
		if time.Now().After(lastCronExecTime.Add(cronFrequency)) {
			core.DeleteExpiredKeys()
			lastCronExecTime = time.Now()
		}

		// Say, the Engine triggered SHUTTING down when the control flow is here ->
		// Current: Engine status == WAITING
		// Update: Engine status = SHUTTING_DOWN
		// Then we have to exit (handled in Signal Handler)

		// poll for events that are ready for IO
		events, err := multiplexer.Poll(-1)
		if err != nil {
			continue
		}

		// Here, we do not want server to go back from SHUTTING DOWN
		// to BUSY
		// If the engine status == SHUTTING_DOWN over here ->
		// We have to exit
		// hence the only legal transitiion is from WAITING to BUSY
		// if that does not happen then we can exit.

		// mark engine as BUSY only when it is in the waiting state
		if !atomic.CompareAndSwapInt32(&eStatus, EngineStatus_WAITING, EngineStatus_BUSY) {
			// if swap unsuccessful then the existing status is not WAITING, but something else
			switch eStatus {
			case EngineStatus_SHUTTING_DOWN:
				return nil
			}
		}

		for _, event := range events {
			// if the socket server itself is ready for an IO
			if event.Fd == serverFD {
				// accept the incoming connection from a client
				fd, _, err := syscall.Accept(serverFD)
				if err != nil {
					log.Println("err", err)
					continue
				}

				connectedClients[fd] = core.NewClient(fd)
				syscall.SetNonblock(fd, true)

				// add this new TCP connection to be monitored
				if err := multiplexer.Subscribe(iomultiplexer.Event{
					Fd: fd,
					Op: iomultiplexer.OP_READ,
				}); err != nil {
					return err
				}
			} else {
				comm := connectedClients[event.Fd]
				if comm == nil {
					continue
				}
				cmds, hasABORT, err := readCommands(comm)

				if err != nil {
					syscall.Close(event.Fd)
					delete(connectedClients, event.Fd)
					continue
				}
				respond(cmds, comm)
				if hasABORT {
					ctx.Done()
					return nil
				}
			}
		}

		// mark engine as WAITING
		// no contention as the signal handler is blocked until
		// the engine is BUSY
		atomic.StoreInt32(&eStatus, EngineStatus_WAITING)
	}

	return nil
}
