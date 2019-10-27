// +build mips mipsle mips64 mips64le

package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/nange/easyss"
	log "github.com/sirupsen/logrus"
)

func StartEasyss(ss *easyss.Easyss) {
	log.Infof("on mips arch, we should ignore systray")

	go ss.Local()     // start local server
	go ss.HttpLocal() // start local http proxy server
	go ss.UDPLocal()  // start local udp server

	c := make(chan os.Signal)
	go func() {
		signal.Notify(c, os.Kill, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM,
			syscall.SIGQUIT)
		log.Infof("receive exit signal:%v", <-c)
	}()

	<-c
	log.Infof("exits easyss...")
}