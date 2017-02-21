package local

import (
	"errors"
	"math/rand"
	"net"
	"sort"
	"time"

	"common"
)

const (
	baseFailCount = 50
	maxFailCount  = 30
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

type loadBalanceMethod func(local net.Conn, rawaddr []byte)

var (
	outboundLoadBalanceHandler          loadBalanceMethod
	outboundIndex                       = 0
	smartLastUsedBackendInfo            *BackendInfo
	forceUpdateSmartLastUsedBackendInfo = false
)

func smartCreateServerConn(local net.Conn, rawaddr []byte, buffer *common.Buffer) (err error) {
	needChangeUsedServerInfo := (smartLastUsedBackendInfo == nil)
	ipv6 := false
	if rawaddr[0] == 4 {
		ipv6 = true
	}
	if forceUpdateSmartLastUsedBackendInfo {
		forceUpdateSmartLastUsedBackendInfo = false
		needChangeUsedServerInfo = true
	} else if smartLastUsedBackendInfo != nil {
		if smartLastUsedBackendInfo.firewalled == true && time.Now().Sub(smartLastUsedBackendInfo.lastCheckTimePoint) < 1*time.Hour {
			common.Warning("firewall dropped")
			goto pick_server
		}
		stat, ok := statistics.Get(smartLastUsedBackendInfo)
		if !ok || stat == nil {
			common.Warning("no statistic record")
			needChangeUsedServerInfo = true
			goto pick_server
		}

		if stat.GetFailedCount() == maxFailCount {
			common.Warning("too many failed count")
			needChangeUsedServerInfo = true
			goto pick_server
		}

		if ipv6 && len(smartLastUsedBackendInfo.ips) > 0 && !smartLastUsedBackendInfo.ipv6 {
			common.Warning("ipv6 is not supported", smartLastUsedBackendInfo.ips)
			goto pick_server
		}

		if remote, err := smartLastUsedBackendInfo.connect(rawaddr); err == nil {
			if inboundSideError, err := smartLastUsedBackendInfo.pipe(local, remote, buffer); err == nil {
				return nil
			} else if inboundSideError {
				common.Info("inbound side error")
				return errors.New("Inbound side error")
			}
			common.Warning("piping failed")
		} else {
			common.Warning("connecting failed")
		}
		if stat.GetFailedCount() < maxFailCount {
			stat.IncreaseFailedCount()
			return errors.New("Smart last used server connecting failed")
		}
	}
pick_server:
	ordered := make(BackendsInformation, 0)
	skipped := make(BackendsInformation, 0)
	{
		backends.RLock()
		for _, bi := range backends.BackendsInformation {
			if bi == smartLastUsedBackendInfo {
				continue
			}

			if ipv6 && len(bi.ips) > 0 && !bi.ipv6 {
				common.Warning("ipv6 is not supported", bi.ips)
				continue
			}

			if bi.firewalled == true && time.Now().Sub(bi.lastCheckTimePoint) < 1*time.Hour {
				continue
			}
			ordered = append(ordered, bi)
		}
		backends.RUnlock()
	}

	sort.Sort(ByHighestLastSecondBps{ordered})
	for _, bi := range ordered {
		// skip failed server, but try it with some probability
		stat, ok := statistics.Get(bi)
		if !ok || stat == nil {
			continue
		}
		if stat.GetFailedCount() == maxFailCount || (stat.GetFailedCount() > 0 && rand.Intn(int(stat.GetFailedCount()+baseFailCount)) != 0) {
			skipped = append(skipped, bi)
			common.Debugf("too large failed count, skip %s\n", bi.address)
			continue
		}
		common.Debugf("try %s with failed count %d, %v, smartLastUsedBackendInfo=%v\n", bi.address, stat.GetFailedCount(), bi, smartLastUsedBackendInfo)

		if remote, err := bi.connect(rawaddr); err == nil {
			if inboundSideError, err := bi.pipe(local, remote, buffer); err == nil || inboundSideError {
				if needChangeUsedServerInfo {
					smartLastUsedBackendInfo = bi
				}
				return nil
			}
		}
		if stat.GetFailedCount() < maxFailCount {
			stat.IncreaseFailedCount()
		}
		common.Debug("try another available server")
	}

	// last resort, try skipped servers, not likely to succeed
	if len(skipped) > 0 {
		sort.Sort(ByLatency{skipped})
		for _, bi := range skipped {
			stat, ok := statistics.Get(bi)
			if !ok || stat == nil {
				continue
			}

			if ipv6 && len(bi.ips) > 0 && !bi.ipv6 {
				common.Warning("ipv6 is not supported", bi.ips)
				continue
			}

			common.Debugf("try %s with failed count %d for an additional optunity, %v\n", bi.address, stat.GetFailedCount(), bi)
			if remote, err := bi.connect(rawaddr); err == nil {
				if inboundSideError, err := bi.pipe(local, remote, buffer); err == nil || inboundSideError {
					if needChangeUsedServerInfo {
						smartLastUsedBackendInfo = bi
					}
					return nil
				}
			}
			if stat.GetFailedCount() < maxFailCount {
				stat.IncreaseFailedCount()
			}
			common.Debug("try another skipped server")
		}
	}

	return errors.New("all servers worked abnormally")
}

func smartLoadBalance(local net.Conn, rawaddr []byte) {
	var buffer *common.Buffer
	maxTryCount := backends.Len()
	for i := 0; i < maxTryCount; i++ {
		err := smartCreateServerConn(local, rawaddr, buffer)
		if err != nil {
			continue
		}

		break
	}
	common.Debug("closed connection to", rawaddr)
}

func indexSpecifiedCreateServerConn(local net.Conn, rawaddr []byte) (remote net.Conn, si *BackendInfo, err error) {
	if backends.Len() == 0 {
		common.Error("no outbound available")
		err = errors.New("no outbound available")
		return
	}
	if outboundIndex >= backends.Len() {
		//common.Warning("the specified index are out of range, use index 0 now")
		outboundIndex = 0
	}
	s := backends.Get(outboundIndex)
	stat, ok := statistics.Get(s)
	if !ok || stat == nil {
		return
	}
	common.Debugf("try %s with failed count %d, %v\n", s.address, stat.GetFailedCount(), s)
	if remote, err = s.connect(rawaddr); err == nil {
		si = s
		return
	}
	if stat.GetFailedCount() < maxFailCount {
		stat.IncreaseFailedCount()
	}
	common.Debug("try another available server")
	return
}

func indexSpecifiedLoadBalance(local net.Conn, rawaddr []byte) {
	var buffer *common.Buffer
	remote, bi, err := indexSpecifiedCreateServerConn(local, rawaddr)
	if err != nil {
		return
	}
	bi.pipe(local, remote, buffer)
	common.Debug("closed connection to", rawaddr)
}

func roundRobinCreateServerConn(local net.Conn, rawaddr []byte) (remote net.Conn, si *BackendInfo, err error) {
	outboundIndex++
	return indexSpecifiedCreateServerConn(local, rawaddr)
}

func roundRobinLoadBalance(local net.Conn, rawaddr []byte) {
	var buffer *common.Buffer
	remote, bi, err := roundRobinCreateServerConn(local, rawaddr)
	if err != nil {
		return
	}
	bi.pipe(local, remote, buffer)
	common.Debug("closed connection to", rawaddr)
}
