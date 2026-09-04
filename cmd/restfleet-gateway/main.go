package main

import (
	"fmt"

	"github.com/sagehou/restfleet/internal/buildinfo"
)

func main() {
	fmt.Printf("restfleet-gateway %s\n", buildinfo.String())
}
