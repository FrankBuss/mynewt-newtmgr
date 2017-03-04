package nmble

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	log "github.com/Sirupsen/logrus"

	"mynewt.apache.org/newt/nmxact/xport"
	"mynewt.apache.org/newt/util/unixchild"
)

type XportCfg struct {
	// Path of Unix domain socket to create and listen on.
	SockPath string

	// Path of the blehostd executable.
	BlehostdPath string

	// Path of the BLE controller device (e.g., /dev/ttyUSB0).
	DevPath string
}

// Implements xport.Xport.
type BleXport struct {
	Bd     *BleDispatcher
	client *unixchild.Client

	syncTimeoutMs time.Duration
}

type BleHostError struct {
	Text   string
	Status int
}

func NewBleHostError(status int, text string) *BleHostError {
	return &BleHostError{
		Status: status,
		Text:   text,
	}
}

func FmtBleHostError(status int, format string,
	args ...interface{}) *BleHostError {

	return NewBleHostError(status, fmt.Sprintf(format, args...))
}

func (e *BleHostError) Error() string {
	return e.Text
}

func IsBleHost(err error) bool {
	_, ok := err.(*BleHostError)
	return ok
}

func ToBleHost(err error) *BleHostError {
	if berr, ok := err.(*BleHostError); ok {
		return berr
	} else {
		return nil
	}
}

func NewBleXport(cfg XportCfg) (*BleXport, error) {
	config := unixchild.Config{
		SockPath:  cfg.SockPath,
		ChildPath: cfg.BlehostdPath,
		ChildArgs: []string{cfg.DevPath, cfg.SockPath},
		Depth:     10,
		MaxMsgSz:  10240,
	}

	c := unixchild.New(config)

	bx := &BleXport{
		client:        c,
		Bd:            NewBleDispatcher(),
		syncTimeoutMs: 10000,
	}

	return bx, nil
}

func (bx *BleXport) addSyncListener() (*BleListener, error) {
	bl := NewBleListener()
	base := BleMsgBase{
		Op:         MSG_OP_EVT,
		Type:       MSG_TYPE_SYNC_EVT,
		Seq:        -1,
		ConnHandle: -1,
	}
	if err := bx.Bd.AddListener(base, bl); err != nil {
		return nil, err
	}

	return bl, nil
}

func (bx *BleXport) querySyncStatus() (bool, error) {
	req := &BleSyncReq{
		Op:   MSG_OP_REQ,
		Type: MSG_TYPE_SYNC,
		Seq:  NextSeq(),
	}

	j, err := json.Marshal(req)
	if err != nil {
		return false, err
	}

	bl := NewBleListener()
	base := BleMsgBase{
		Op:         -1,
		Type:       -1,
		Seq:        req.Seq,
		ConnHandle: -1,
	}
	if err := bx.Bd.AddListener(base, bl); err != nil {
		return false, err
	}
	defer bx.Bd.RemoveListener(base)

	bx.txNoSync(j)
	for {
		select {
		case err := <-bl.ErrChan:
			return false, err
		case bm := <-bl.BleChan:
			switch msg := bm.(type) {
			case *BleSyncRsp:
				return msg.Synced, nil
			}
		}
	}
}

func (bx *BleXport) onError(err error) {
	bx.Bd.ErrorAll(err)
	bx.Stop()
}

func (bx *BleXport) Start() error {
	if err := bx.client.Start(); err != nil {
		return xport.NewXportError(
			"Failed to start child child process: " + err.Error())
	}

	go func() {
		err := <-bx.client.ErrChild
		bx.onError(err)
	}()

	go func() {
		for {
			if _, err := bx.rx(); err != nil {
				// The error should have been reported to everyone interested.
				break
			}
		}
	}()

	bl, err := bx.addSyncListener()
	if err != nil {
		return nil
	}

	synced, err := bx.querySyncStatus()
	if err != nil {
		return err
	}

	if !synced {
		// Not synced yet.  Wait for sync event.
		tmoChan := make(chan struct{}, 1)
		go func() {
			time.Sleep(bx.syncTimeoutMs * time.Millisecond)
			tmoChan <- struct{}{}
		}()

	SyncLoop:
		for {
			select {
			case <-tmoChan:
				return xport.NewXportError(
					"Timeout waiting for host <-> controller sync")
			case err := <-bl.ErrChan:
				return err
			case bm := <-bl.BleChan:
				switch msg := bm.(type) {
				case *BleSyncEvt:
					if msg.Synced {
						break SyncLoop
					}
				}
			}
		}
	}

	// Host and controller are synced.  Listen for sync loss in the background.
	go func() {
		for {
			select {
			case err := <-bl.ErrChan:
				bx.onError(err)
				return
			case bm := <-bl.BleChan:
				switch msg := bm.(type) {
				case *BleSyncEvt:
					if !msg.Synced {
						bx.onError(xport.NewXportError(
							"BLE host <-> controller sync lost"))
						return
					}
				}
			}
		}
	}()

	return nil
}

func (bx *BleXport) txNoSync(data []byte) {
	log.Debugf("Tx to blehostd:\n%s", hex.Dump(data))
	bx.client.ToChild <- data
}

func (bx *BleXport) Tx(data []byte) error {
	// XXX: Make sure started.
	bx.txNoSync(data)
	return nil
}

func (bx *BleXport) rx() ([]byte, error) {
	select {
	case err := <-bx.client.ErrChild:
		return nil, err
	case buf := <-bx.client.FromChild:
		if len(buf) != 0 {
			log.Debugf("Receive from blehostd:\n%s", hex.Dump(buf))
			bx.Bd.Dispatch(buf)
		}
		return buf, nil
	}
}

func (bx *BleXport) Stop() error {
	if bx.client != nil {
		bx.client.FromChild <- nil
		bx.client.Stop()
	}

	return nil
}
