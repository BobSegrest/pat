// Copyright 2016 Martin Hebnes Pedersen (LA5NTA). All rights reserved.
// Use of this source code is governed by the MIT-license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/la5nta/pat/cfg"
	"github.com/la5nta/pat/internal/debug"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/harenber/ptc-go/v2/pactor"
	"github.com/la5nta/wl2k-go/transport"
	"github.com/la5nta/wl2k-go/transport/ardop"
	"github.com/n8jja/Pat-Vara/vara"

	// Register other dialers
	_ "github.com/la5nta/wl2k-go/transport/ax25"
	_ "github.com/la5nta/wl2k-go/transport/telnet"
)

var (
	dialing     *transport.URL // The connect URL currently being dialed (if any)
	adTNC       *ardop.TNC     // Pointer to the ARDOP TNC used by Listen and Connect
	pModem      *pactor.Modem
	varaHFModem *vara.Modem
	varaFMModem *vara.Modem

	// Context cancellation function for aborting while dialing.
	dialCancelFunc func() = func() {}
)

func hasSSID(str string) bool { return strings.Contains(str, "-") }

func connectAny(connectStr ...string) bool {
	for _, str := range connectStr {
		if Connect(str) {
			return true
		}
	}
	return false
}

func Connect(connectStr string) (success bool) {
	if connectStr == "" {
		return false
	} else if aliased, ok := config.ConnectAliases[connectStr]; ok {
		return Connect(aliased)
	}

	// Hack around bug in frontend which may occur if the status updates too quickly.
	defer func() { time.Sleep(time.Second); websocketHub.UpdateStatus() }()

	debug.Printf("connectStr: %s", connectStr)
	url, err := transport.ParseURL(connectStr)
	if err != nil {
		log.Println(err)
		return false
	}

	// Init TNCs
	switch url.Scheme {
	case MethodArdop:
		if err := initArdopTNC(); err != nil {
			log.Println(err)
			return
		}
	case MethodPactor:
		ptCmdInit := ""
		if val, ok := url.Params["init"]; ok {
			ptCmdInit = strings.Join(val, "\n")
		}
		if err := initPactorModem(ptCmdInit); err != nil {
			log.Println(err)
			return
		}
	case MethodVaraHF:
		if err := initVaraModem(varaHFModem, MethodVaraHF, config.VaraHF); err != nil {
			log.Println(err)
			return
		}
	case MethodVaraFM:
		if err := initVaraModem(varaFMModem, MethodVaraFM, config.VaraFM); err != nil {
			log.Println(err)
			return
		}
	}

	// Set default userinfo (mycall)
	if url.User == nil {
		url.SetUser(fOptions.MyCall)
	}

	// Set default host interface address
	if url.Host == "" {
		switch url.Scheme {
		case MethodAX25:
			url.Host = config.AX25.Port
		case MethodSerialTNC:
			url.Host = config.SerialTNC.Path
			if hbaud := config.SerialTNC.HBaud; hbaud > 0 {
				url.Params.Set("hbaud", fmt.Sprint(hbaud))
			}
			if sbaud := config.SerialTNC.SerialBaud; sbaud > 0 {
				url.Params.Set("serial_baud", fmt.Sprint(sbaud))
			}
		}
	}

	// Radio Only?
	radioOnly := fOptions.RadioOnly
	if v := url.Params.Get("radio_only"); v != "" {
		radioOnly, _ = strconv.ParseBool(v)
	}
	if radioOnly {
		if hasSSID(fOptions.MyCall) {
			log.Println("Radio Only does not support callsign with SSID")
			return
		}

		switch url.Scheme {
		case MethodAX25, MethodSerialTNC:
			log.Printf("Radio-Only is not available for %s", url.Scheme)
			return
		default:
			url.SetUser(url.User.Username() + "-T")
		}
	}

	// QSY
	var revertFreq func()
	if freq := url.Params.Get("freq"); freq != "" {
		revertFreq, err = qsy(url.Scheme, freq)
		if err != nil {
			log.Printf("Unable to QSY: %s", err)
			return
		}
		defer revertFreq()
	}
	var currFreq Frequency
	if vfo, _, ok, _ := VFOForTransport(url.Scheme); ok {
		f, _ := vfo.GetFreq()
		currFreq = Frequency(f)
	}

	// Wait for a clear channel
	switch url.Scheme {
	case MethodArdop:
		waitBusy(adTNC)
	}

	ctx, cancel := context.WithCancel(context.Background())
	dialCancelFunc = func() { dialing = nil; cancel() }
	defer dialCancelFunc()

	// Signal web gui that we are dialing a connection
	dialing = url
	websocketHub.UpdateStatus()

	log.Printf("Connecting to %s (%s)...", url.Target, url.Scheme)
	conn, err := transport.DialURLContext(ctx, url)

	// Signal web gui that we are no longer dialing
	dialing = nil
	websocketHub.UpdateStatus()

	eventLog.LogConn("connect "+connectStr, currFreq, conn, err)

	switch {
	case errors.Is(err, context.Canceled):
		log.Printf("Connect cancelled")
		return
	case err != nil:
		log.Printf("Unable to establish connection to remote: %s", err)
		return
	}

	err = exchange(conn, url.Target, false)
	if err != nil {
		log.Printf("Exchange failed: %s", err)
	} else {
		log.Println("Disconnected.")
		success = true
	}

	return
}

func qsy(method, addr string) (revert func(), err error) {
	noop := func() {}
	rig, rigName, ok, err := VFOForTransport(method)
	if err != nil {
		return noop, err
	} else if !ok {
		return noop, fmt.Errorf("hamlib rig '%s' not loaded", rigName)
	}

	log.Printf("QSY %s: %s", method, addr)
	_, oldFreq, err := setFreq(rig, addr)
	if err != nil {
		return noop, err
	}

	time.Sleep(3 * time.Second)
	return func() {
		time.Sleep(time.Second)
		log.Printf("QSX %s: %.3f", method, float64(oldFreq)/1e3)
		rig.SetFreq(oldFreq)
	}, nil
}

func waitBusy(b transport.BusyChannelChecker) {
	printed := false

	for b.Busy() {
		if !printed && fOptions.IgnoreBusy {
			log.Println("Ignoring busy channel!")
			break
		} else if !printed {
			log.Println("Waiting for clear channel...")
			printed = true
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func initArdopTNC() error {
	if adTNC != nil && adTNC.Ping() == nil {
		return nil
	}

	if adTNC != nil {
		adTNC.Close()
	}

	var err error
	adTNC, err = ardop.OpenTCP(config.Ardop.Addr, fOptions.MyCall, config.Locator)
	if err != nil {
		return fmt.Errorf("ARDOP TNC initialization failed: %w", err)
	}

	if !config.Ardop.ARQBandwidth.IsZero() {
		if err := adTNC.SetARQBandwidth(config.Ardop.ARQBandwidth); err != nil {
			return fmt.Errorf("unable to set ARQ bandwidth for ardop TNC: %w", err)
		}
	}

	if err := adTNC.SetCWID(config.Ardop.CWID); err != nil {
		return fmt.Errorf("unable to configure CWID for ardop TNC: %w", err)
	}

	if v, err := adTNC.Version(); err != nil {
		return fmt.Errorf("ARDOP TNC initialization failed: %s", err)
	} else {
		log.Printf("ARDOP TNC (%s) initialized", v)
	}

	transport.RegisterDialer(MethodArdop, adTNC)

	if !config.Ardop.PTTControl {
		return nil
	}

	rig, ok := rigs[config.Ardop.Rig]
	if !ok {
		return fmt.Errorf("unable to set PTT rig '%s': Not defined or not loaded", config.Ardop.Rig)
	}

	adTNC.SetPTT(rig)
	return nil
}

func initPactorModem(cmdlineinit string) error {
	if pModem != nil {
		pModem.Close()
	}
	var err error
	pModem, err = pactor.OpenModem(config.Pactor.Path, config.Pactor.Baudrate, fOptions.MyCall, config.Pactor.InitScript, cmdlineinit)
	if err != nil || pModem == nil {
		return fmt.Errorf("pactor initialization failed: %w", err)
	}

	transport.RegisterDialer(MethodPactor, pModem)

	return nil
}

func initVaraModem(vModem *vara.Modem, scheme string, conf cfg.VaraConfig) error {
	if vModem != nil {
		_ = vModem.Close()
	}
	vConf := vara.ModemConfig{
		Host:     conf.Host,
		CmdPort:  conf.CmdPort,
		DataPort: conf.DataPort,
	}
	var err error
	vModem, err = vara.NewModem(scheme, fOptions.MyCall, vConf)
	if err != nil {
		return fmt.Errorf("vara initialization failed: %w", err)
	}

	transport.RegisterDialer(scheme, vModem)

	if !conf.PTTControl {
		return nil
	}

	rig, ok := rigs[conf.Rig]
	if !ok {
		return fmt.Errorf("unable to set PTT rig '%s': not defined or not loaded", conf.Rig)
	}
	vModem.SetPTT(rig)
	return nil
}
