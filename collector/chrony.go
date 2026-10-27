// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !nochrony

package collector

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
)

const chronySubsystem = "chrony"

// Subset of the chronyd command protocol (candm.h) needed for REQ_TRACKING,
// the request behind `chronyc tracking`.
const (
	chronyProtoVersion   = 6
	chronyPktTypeRequest = 1
	chronyPktTypeReply   = 2
	chronyReqTracking    = 33
	chronyRpyTracking    = 5
	chronyStatusSuccess  = 0

	chronyReplyHeaderLen = 28
	// RPY_Tracking up to its EOR marker.
	chronyTrackingLen      = 76
	chronyTrackingReplyLen = chronyReplyHeaderLen + chronyTrackingLen

	// Timespec.tv_sec_high value meaning "no high bits".
	chronyNoHighSec = 0x7fffffff
)

var (
	chronyAddress = kingpin.Flag("collector.chrony.address", "UDP address of the chronyd command port, as used by chronyc").Default("127.0.0.1:323").String()
	chronyTimeout = kingpin.Flag("collector.chrony.timeout", "Timeout for requests to chronyd").Default("1s").Duration()
)

type chronyCollector struct {
	systemTimeOffset typedDesc
	lastOffset       typedDesc
	rootDelay        typedDesc
	rootDispersion   typedDesc
	stratum          typedDesc
	refTime          typedDesc
	logger           *slog.Logger
}

type chronyTracking struct {
	stratum          uint16
	refTime          float64
	systemTimeOffset float64
	lastOffset       float64
	rootDelay        float64
	rootDispersion   float64
}

func init() {
	registerCollector("chrony", defaultEnabled, NewChronyCollector)
}

// NewChronyCollector returns a new Collector exposing the clock offset and
// synchronisation state reported by chronyd.
func NewChronyCollector(logger *slog.Logger) (Collector, error) {
	return &chronyCollector{
		systemTimeOffset: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, chronySubsystem, "system_time_offset_seconds"),
			"Offset of the system clock from chronyd's estimate of true time (System time in chronyc tracking). Positive means the system clock is slow.",
			nil, nil,
		), prometheus.GaugeValue},
		lastOffset: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, chronySubsystem, "last_offset_seconds"),
			"Estimated offset of the system clock at the last clock update. Positive means the system clock was fast.",
			nil, nil,
		), prometheus.GaugeValue},
		rootDelay: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, chronySubsystem, "root_delay_seconds"),
			"Total network path delay to the stratum-1 source chronyd is synchronised to.",
			nil, nil,
		), prometheus.GaugeValue},
		rootDispersion: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, chronySubsystem, "root_dispersion_seconds"),
			"Total dispersion accumulated back to the stratum-1 source chronyd is synchronised to.",
			nil, nil,
		), prometheus.GaugeValue},
		stratum: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, chronySubsystem, "stratum"),
			"Stratum of the system clock, 0 when chronyd is not synchronised.",
			nil, nil,
		), prometheus.GaugeValue},
		refTime: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, chronySubsystem, "reference_timestamp_seconds"),
			"Unix time of the last clock update, 0 if chronyd has never synchronised.",
			nil, nil,
		), prometheus.GaugeValue},
		logger: logger,
	}, nil
}

func (c *chronyCollector) Update(ch chan<- prometheus.Metric) error {
	t, err := queryChronyTracking(*chronyAddress, *chronyTimeout)
	if err != nil {
		// Nothing listening on the command port: chronyd isn't running here.
		if errors.Is(err, syscall.ECONNREFUSED) {
			c.logger.Debug("chronyd is not listening", "address", *chronyAddress, "err", err)
			return ErrNoData
		}
		return fmt.Errorf("couldn't get chrony tracking data from %s: %w", *chronyAddress, err)
	}

	ch <- c.systemTimeOffset.mustNewConstMetric(t.systemTimeOffset)
	ch <- c.lastOffset.mustNewConstMetric(t.lastOffset)
	ch <- c.rootDelay.mustNewConstMetric(t.rootDelay)
	ch <- c.rootDispersion.mustNewConstMetric(t.rootDispersion)
	ch <- c.stratum.mustNewConstMetric(float64(t.stratum))
	ch <- c.refTime.mustNewConstMetric(t.refTime)
	return nil
}

func queryChronyTracking(address string, timeout time.Duration) (*chronyTracking, error) {
	conn, err := net.DialTimeout("udp", address, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}

	// chronyd ignores requests shorter than the reply they ask for, so pad
	// the request up to the size of the tracking reply.
	seq := rand.Uint32()
	req := make([]byte, chronyTrackingReplyLen)
	req[0] = chronyProtoVersion
	req[1] = chronyPktTypeRequest
	binary.BigEndian.PutUint16(req[4:], chronyReqTracking)
	binary.BigEndian.PutUint32(req[8:], seq)
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}

	resp := make([]byte, 1024)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, err
	}
	return parseChronyTrackingReply(resp[:n], seq)
}

func parseChronyTrackingReply(b []byte, seq uint32) (*chronyTracking, error) {
	if len(b) < chronyReplyHeaderLen {
		return nil, fmt.Errorf("reply too short: %d bytes", len(b))
	}
	if b[0] != chronyProtoVersion || b[1] != chronyPktTypeReply ||
		binary.BigEndian.Uint16(b[4:]) != chronyReqTracking ||
		binary.BigEndian.Uint32(b[16:]) != seq {
		return nil, fmt.Errorf("unexpected reply (version %d, type %d, command %d)", b[0], b[1], binary.BigEndian.Uint16(b[4:]))
	}
	if status := binary.BigEndian.Uint16(b[8:]); status != chronyStatusSuccess {
		return nil, fmt.Errorf("chronyd returned status %d", status)
	}
	if rpy := binary.BigEndian.Uint16(b[6:]); rpy != chronyRpyTracking || len(b) < chronyTrackingReplyLen {
		return nil, fmt.Errorf("unexpected tracking reply (reply %d, %d bytes)", rpy, len(b))
	}

	// RPY_Tracking: ref_id(4) ip_addr(20) stratum(2) leap_status(2)
	// ref_time(12) followed by nine chrony Floats.
	d := b[chronyReplyHeaderLen:]
	secHigh := binary.BigEndian.Uint32(d[28:])
	if secHigh == chronyNoHighSec {
		secHigh = 0
	}
	sec := uint64(secHigh)<<32 | uint64(binary.BigEndian.Uint32(d[32:]))
	nsec := binary.BigEndian.Uint32(d[36:])

	return &chronyTracking{
		stratum:          binary.BigEndian.Uint16(d[24:]),
		refTime:          float64(sec) + float64(nsec)/1e9,
		systemTimeOffset: chronyFloat(d[40:]),
		lastOffset:       chronyFloat(d[44:]),
		rootDelay:        chronyFloat(d[64:]),
		rootDispersion:   chronyFloat(d[68:]),
	}, nil
}

// chronyFloat decodes chrony's 32-bit network float: a 7-bit signed exponent
// and a 25-bit signed coefficient, value = coef * 2^(exp-25).
func chronyFloat(b []byte) float64 {
	x := binary.BigEndian.Uint32(b)
	exp := int32(x) >> 25
	coef := int32(x<<7) >> 7
	return math.Ldexp(float64(coef), int(exp)-25)
}
