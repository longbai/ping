package main

import (
	"flag"
	"fmt"
	"net/http"

	h "github.com/qiniu/httpping/http"
)

func main() {
	url := flag.String("u", "www.baidu.com", "ping url")
	ping := flag.Bool("p", true, "with system ping command")
	local := flag.String("l", "", "local address")
	range_ := flag.String("r", "", "http range")
	flag.Parse()

	req, err := http.NewRequest(http.MethodGet, *url, nil)
	if err != nil {
		fmt.Println(err)
		flag.PrintDefaults()
		return
	}
	if *range_ != "" {
		req.Header.Set("Range", "bytes="+*range_)
	}

	info, err := h.HttpPing(req, *ping, *local)
	if err != nil {
		fmt.Println(err)
		flag.PrintDefaults()
		return
	}

	fmt.Println(info.String())
}
