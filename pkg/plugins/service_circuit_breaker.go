package plugins

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/megaease/easegateway/pkg/common"
	"github.com/megaease/easegateway/pkg/logger"

	"github.com/megaease/easegateway/pkg/pipelines"

	"github.com/megaease/easegateway/pkg/task"
)

type serviceCircuitBreakerConfig struct {
	PluginCommonConfig
	PluginsConcerned []string `json:"plugins_concerned"`
	// condition to enable circuit breaker
	AllTPSThresholdToEnablement float64 `json:"all_tps_threshold_to_enable"`
	// conditions to turns circuit breaker open (fully close request flow)
	FailureTPSThresholdToBreak        float64 `json:"failure_tps_threshold_to_break"`
	FailureTPSPercentThresholdToBreak float32 `json:"failure_tps_percent_threshold_to_break"`
	// condition to turns circuit breaker half-open (try service availability)
	RecoveryTimeMSec uint32 `json:"recovery_time_msec"` // up to 4294967295, equals to MTTR generally
	// condition to turns circuit breaker closed (fully open request flow)
	SuccessTPSThresholdToOpen float64 `json:"success_tps_threshold_to_open"`
}

func serviceCircuitBreakerConfigConstructor() Config {
	return &serviceCircuitBreakerConfig{
		AllTPSThresholdToEnablement: 1,
		FailureTPSThresholdToBreak:  1,
		RecoveryTimeMSec:            1000,
		SuccessTPSThresholdToOpen:   1,
	}
}

func (c *serviceCircuitBreakerConfig) Prepare(pipelineNames []string) error {
	err := c.PluginCommonConfig.Prepare(pipelineNames)
	if err != nil {
		return err
	}

	if len(c.PluginsConcerned) == 0 {
		return fmt.Errorf("invalid plugins concerned")
	}

	for _, pluginName := range c.PluginsConcerned {
		if len(strings.TrimSpace(pluginName)) == 0 {
			return fmt.Errorf("invalid plugin name")
		}
	}

	if c.AllTPSThresholdToEnablement < 0 {
		// Equals to zero means to enable circuit breaker immediately when a request arrived.
		return fmt.Errorf("invalid all throughput rate threshold to enable cricuit breaker")
	}

	if c.RecoveryTimeMSec < 1 {
		return fmt.Errorf("invalid recovery time")
	}

	if c.SuccessTPSThresholdToOpen < 0 {
		// Equals to zero means to fully open request flow immediately after recovery time elapsed.
		return fmt.Errorf("invalid success throughput rate threshold to open request")
	}

	if c.FailureTPSThresholdToBreak == 0 || c.FailureTPSPercentThresholdToBreak == 0 {
		logger.Warnf("[ZERO failure throughput rate or throughput rate percentage threshold " +
			"has been applied, breaker will keep open or half-open!]")
	}

	return nil
}

////

type serviceCircuitBreaker struct {
	conf       *serviceCircuitBreakerConfig
	instanceId string
}

func serviceCircuitBreakerConstructor(conf Config) (Plugin, PluginType, bool, error) {
	c, ok := conf.(*serviceCircuitBreakerConfig)
	if !ok {
		return nil, ProcessPlugin, false, fmt.Errorf(
			"config type want *serviceCircuitBreakerConfig got %T", conf)
	}

	cb := &serviceCircuitBreaker{
		conf: c,
	}

	cb.instanceId = fmt.Sprintf("%p", cb)

	return cb, ProcessPlugin, false, nil
}

func (cb *serviceCircuitBreaker) Prepare(ctx pipelines.PipelineContext) {
	callbackName := fmt.Sprintf(
		"%s-pluginExecutionSampleUpdatedForPluginInstance@%p", cb.Name(), cb)

	ctx.Statistics().AddPluginExecutionSampleUpdatedCallback(
		callbackName,
		cb.getPluginExecutionSampleUpdatedCallback(ctx))
}

func (cb *serviceCircuitBreaker) Run(ctx pipelines.PipelineContext, t task.Task) error {
	state, err := getServiceCircuitBreakerStateData(ctx, cb.conf.PluginsConcerned,
		cb.conf.AllTPSThresholdToEnablement, cb.conf.FailureTPSThresholdToBreak,
		cb.conf.FailureTPSPercentThresholdToBreak, cb.conf.SuccessTPSThresholdToOpen,
		cb.Name(), cb.instanceId)
	if err != nil {
		return nil
	}

	state.Lock()
	defer state.Unlock()

	switch state.status {
	case off:
		fallthrough
	case closed:
		return nil
	case open:
		if common.Since(state.openAt).Seconds()*1e3 <= float64(cb.conf.RecoveryTimeMSec) {
			// service fusing
			t.SetError(fmt.Errorf("service is unavailable caused by service fusing"),
				task.ResultFlowControl)
		} else { // recovery timeout, turns to half-open
			state.status = halfOpen
			state.halfOpenAt = common.Now()
			state.openAt = time.Time{}
			logger.Debugf("[service circuit breaker turns status from Open to %s "+
				"(recovery %dms timeout)", state.status, cb.conf.RecoveryTimeMSec)
		}
		return nil
	case halfOpen:
		// try task
		return nil
	}

	return fmt.Errorf("BUG: unreasonable execution path")
}

func (cb *serviceCircuitBreaker) Name() string {
	return cb.conf.PluginName()
}

func (cb *serviceCircuitBreaker) CleanUp(ctx pipelines.PipelineContext) {
	callbackName := fmt.Sprintf(
		"%s-pluginExecutionSampleUpdatedForPluginInstance@%p", cb.Name(), cb)
	ctx.Statistics().DeletePluginExecutionSampleUpdatedCallback(callbackName)

	ctx.DeleteBucket(cb.Name(), cb.instanceId)
}

func (cb *serviceCircuitBreaker) Close() {
	// Nothing to do.
}

func (cb *serviceCircuitBreaker) getPluginExecutionSampleUpdatedCallback(
	ctx pipelines.PipelineContext) pipelines.PluginExecutionSampleUpdated {

	return func(pluginName string, latestStatistics pipelines.PipelineStatistics,
		kind pipelines.StatisticsKind) {

		if !common.StrInSlice(pluginName, cb.conf.PluginsConcerned) {
			return // ignore safely
		}

		state, err := getServiceCircuitBreakerStateData(ctx, cb.conf.PluginsConcerned,
			cb.conf.AllTPSThresholdToEnablement, cb.conf.FailureTPSThresholdToBreak,
			cb.conf.FailureTPSPercentThresholdToBreak, cb.conf.SuccessTPSThresholdToOpen,
			cb.Name(), cb.instanceId)
		if err != nil {
			return
		}

		state.Lock()
		defer state.Unlock()

		newStatus := nextStatus(ctx, cb.conf.PluginsConcerned, state.status, state.halfOpenAt,
			cb.conf.AllTPSThresholdToEnablement, cb.conf.FailureTPSThresholdToBreak,
			cb.conf.FailureTPSPercentThresholdToBreak, cb.conf.SuccessTPSThresholdToOpen)
		if newStatus == state.status {
			return
		}

		if newStatus == open {
			state.openAt = common.Now()
		} else if newStatus == halfOpen {
			state.halfOpenAt = common.Now()
		}

		if state.status == halfOpen {
			state.halfOpenAt = time.Time{}
		}

		state.status = newStatus
		return
	}
}

////

const (
	serviceCircuitBreakerStateDataKey = "serviceCircuitBreakerStateDataKey"
)

type circuitBreakerStatus string

const (
	off      circuitBreakerStatus = "Off"
	closed   circuitBreakerStatus = "Closed"
	halfOpen circuitBreakerStatus = "HalfOpen"
	open     circuitBreakerStatus = "Open"
)

type circuitBreakerStateData struct {
	sync.Mutex
	status             circuitBreakerStatus
	openAt, halfOpenAt time.Time
}

// FIXME: squeeze arguments
func getServiceCircuitBreakerStateData(ctx pipelines.PipelineContext, pluginsConcerned []string,
	tpsToEnablement, tpsToBreak float64, tpsPercentToBreak float32, tpsToOpen float64,
	pluginName, pluginInstanceId string) (*circuitBreakerStateData, error) {

	bucket := ctx.DataBucket(pluginName, pluginInstanceId)
	state, err := bucket.QueryDataWithBindDefault(serviceCircuitBreakerStateDataKey,
		func() interface{} {
			var openAt time.Time

			status := nextStatus(ctx, pluginsConcerned, off, time.Time{}, tpsToEnablement,
				tpsToBreak, tpsPercentToBreak, tpsToOpen)
			if status == open {
				openAt = common.Now()
			}

			return &circuitBreakerStateData{
				status: status,
				openAt: openAt,
			}
		})

	if err != nil {
		logger.Warnf("[BUG: query state data for pipeline %s failed, "+
			"ignored to handle service fusing: %v]", ctx.PipelineName(), err)
		return nil, err
	}

	return state.(*circuitBreakerStateData), nil
}

func getTPS(ctx pipelines.PipelineContext, pluginsConcerned []string,
	tpsQuerier func(pluginName string, kind pipelines.StatisticsKind) (float64, error),
	kind pipelines.StatisticsKind) float64 {

	var ret float64

	for _, name := range pluginsConcerned {
		if !common.StrInSlice(name, ctx.PluginNames()) {
			continue // ignore safely
		}

		tps, err := tpsQuerier(name, kind)
		if err != nil {
			logger.Warnf("[BUG: query plugin %s throughput rate failed (kind=%s), "+
				"ignored to consider the rate of this plugin: %v]", name, kind, err)
			continue
		}

		if tps < 0 {
			continue // doesn't make sense, defensive
		}

		ret += tps
	}

	return ret
}

// FIXME: squeeze arguments
func nextStatus(ctx pipelines.PipelineContext, pluginsConcerned []string, currentStatus circuitBreakerStatus,
	halfOpenAt time.Time, tpsToEnablement, tpsToBreak float64, tpsPercentToBreak float32,
	tpsToOpen float64) circuitBreakerStatus {

	var ret circuitBreakerStatus = off

	allTps5 := getTPS(ctx, pluginsConcerned,
		ctx.Statistics().PluginThroughputRate1, // value 1 is an option?
		pipelines.AllStatistics)

	switch currentStatus {
	case off: // check if turns to closed or open directly
		if allTps5 >= tpsToEnablement {
			ret = closed
			logger.Debugf("[service circuit breaker turns status from %s to %s (all tps %f >= %f)",
				currentStatus, ret, allTps5, tpsToEnablement)
		}
		fallthrough
	case closed: // check if turns to open or off
		failureTps := getTPS(ctx, pluginsConcerned,
			ctx.Statistics().PluginThroughputRate1, // value 1 is an option?
			pipelines.FailureStatistics)
		allTps1 := getTPS(ctx, pluginsConcerned,
			ctx.Statistics().PluginThroughputRate1,
			pipelines.AllStatistics)

		// tpsToBreak equals to zero means no request could be processed, allows operator to stop all request
		if tpsToBreak >= 0 && failureTps >= tpsToBreak {
			ret = open
			logger.Debugf("[service circuit breaker turns status from %s to %s "+
				"(failure tps %f >= %f)", currentStatus, ret, failureTps, tpsToBreak)
		} else if (tpsPercentToBreak >= 0 || tpsPercentToBreak <= 100) &&
			failureTps/allTps1*100 >= float64(tpsPercentToBreak) {
			ret = open
			logger.Debugf("[service circuit breaker turns status from %s to %s "+
				"(failure tps %f >= %f)", currentStatus, ret,
				failureTps/allTps1*100, tpsPercentToBreak)
		} else if allTps5 < tpsToEnablement {
			ret = off
			logger.Debugf("[service circuit breaker turns status from %s to %s "+
				"(all tps %f < %f)", currentStatus, ret, allTps5, tpsToEnablement)
		} else {
			ret = closed
		}
	case open:
		// Nothing to do, Run() checks if turns to half-open status
		ret = open
	case halfOpen: // check if turns to open or closed
		// TODO: Uses execution count in the unit time to determine next status
		// instead of using success tps if needed
		if common.Since(halfOpenAt).Minutes() < 1 {
			ret = halfOpen
			break
		}

		successTps := getTPS(ctx, pluginsConcerned,
			ctx.Statistics().PluginThroughputRate1, // value 1 is an option?
			pipelines.SuccessStatistics)
		if successTps >= tpsToOpen {
			ret = closed
			logger.Debugf("[service circuit breaker turns from status %s to %s "+
				"(success tps %f >= %f)", currentStatus, ret, successTps, tpsToOpen)
		} else {
			ret = open
			logger.Debugf("[service circuit breaker turns status from %s to %s "+
				"(success tps %f < %f)", currentStatus, ret, successTps, tpsToOpen)
		}
	}

	return ret
}