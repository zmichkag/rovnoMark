//go:build !windows

package bizerba

import "fmt"

func newBCSConnection() (bcsConnection, error) {
	return nil, fmt.Errorf("COM-драйвер Bizerba BCS доступен только в Windows")
}
