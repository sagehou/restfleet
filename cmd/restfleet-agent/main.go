package main

import (
	"fmt"

	"github.com/sagehou/restfleet/internal/buildinfo"
)

func main() {
	fmt.Printf("restfleet-agent %s\n", buildinfo.String())
}
