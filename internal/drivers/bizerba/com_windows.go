//go:build windows

package bizerba

import (
	"fmt"
	"time"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

const bcsProgramID = "BCS.BCSComunnication.1"

type comConnection struct {
	dispatch       *ole.IDispatch
	comInitialized bool
	opened         bool
}

func newBCSConnection() (bcsConnection, error) {
	c := &comConnection{}
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		return nil, fmt.Errorf("ошибка инициализации COM: %w", err)
	}
	c.comInitialized = true
	unknown, err := oleutil.CreateObject(bcsProgramID)
	if err != nil {
		c.release()
		return nil, fmt.Errorf("не удалось создать COM-объект %s: %w", bcsProgramID, err)
	}
	dispatch, err := unknown.QueryInterface(ole.IID_IDispatch)
	unknown.Release()
	if err != nil {
		c.release()
		return nil, fmt.Errorf("не удалось получить IDispatch для %s: %w", bcsProgramID, err)
	}
	c.dispatch = dispatch
	return c, nil
}

func (c *comConnection) Open(identity, device string, spontaneous bool) error {
	spontaneousValue := int16(0)
	if spontaneous {
		spontaneousValue = 1
	}
	result, err := oleutil.CallMethod(c.dispatch, "Open", identity, device, spontaneousValue, int16(0), int16(0))
	if err != nil {
		return fmt.Errorf("ошибка вызова BCS Open(%q): %w", device, err)
	}
	defer result.Clear()
	if code := int(result.Val); code != 0 {
		return fmt.Errorf("BCS Open(%q): получен код %d", device, code)
	}
	c.opened = true
	return nil
}

func (c *comConnection) Send(header, data string, timeout time.Duration) (string, int, error) {
	var handle string
	var status int32
	result, err := oleutil.CallMethod(c.dispatch, "Send", header, data, &handle, int32(timeout.Milliseconds()), &status)
	if err != nil {
		return "", int(status), fmt.Errorf("ошибка вызова BCS Send(%q): %w", header, err)
	}
	result.Clear()
	return handle, int(status), nil
}

func (c *comConnection) ReceiveOne(queue string, timeout time.Duration) (string, int, error) {
	var payload string
	var status int32
	result, err := oleutil.CallMethod(c.dispatch, "ReceiveOne", &payload, queue, int32(timeout.Milliseconds()), &status)
	if err != nil {
		return "", int(status), fmt.Errorf("ошибка вызова BCS ReceiveOne(%q): %w", queue, err)
	}
	result.Clear()
	return payload, int(status), nil
}

func (c *comConnection) Close() error {
	var closeErr error
	if c.opened && c.dispatch != nil {
		result, err := oleutil.CallMethod(c.dispatch, "Close")
		if err != nil {
			closeErr = fmt.Errorf("ошибка вызова BCS Close: %w", err)
		} else {
			result.Clear()
		}
		c.opened = false
	}
	c.release()
	return closeErr
}

func (c *comConnection) release() {
	if c.dispatch != nil {
		c.dispatch.Release()
		c.dispatch = nil
	}
	if c.comInitialized {
		ole.CoUninitialize()
		c.comInitialized = false
	}
}
