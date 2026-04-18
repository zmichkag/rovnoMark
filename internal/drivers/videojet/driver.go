package videojet

import (
	"fmt"
	"net"
	"time"
)

type Driver struct {
	Address string
	Timeout time.Duration
}

func New(ip string) *Driver {
	return &Driver{
		Address: ip + ":3002",
		Timeout: 3 * time.Second,
	}
}

func (d *Driver) SendCommand(cmdBody string) (string, error) {
	conn, err := net.DialTimeout("tcp", d.Address, d.Timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	conn.Write([]byte(fmt.Sprintf("~%s^", cmdBody)))
	conn.SetReadDeadline(time.Now().Add(d.Timeout))
	buffer := make([]byte, 1024)
	n, err := conn.Read(buffer)
	if err != nil {
		return "", err
	}
	return string(buffer[:n]), nil
}
