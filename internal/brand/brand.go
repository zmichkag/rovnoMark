package brand

import (
	"os"
)

var (
	Name       = "OpenMark"
	Vendor     = "Industrial Gateway"
	SupportURL = "support@local.net"
)

func GetName() string {
	if val := os.Getenv("GATEWAY_BRAND_NAME"); val != "" {
		return val
	}
	return Name
}
