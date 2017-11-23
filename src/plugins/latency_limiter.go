package plugins

import (
	"fmt"
	"strings"
	"time"

	"github.com/hexdecteam/easegateway-types/pipelines"
	"github.com/hexdecteam/easegateway-types/plugins"
	"github.com/hexdecteam/easegateway-types/task"

	"common"
	"logger"
)

type latencyLimiterConfig struct {
	common.PluginCommonConfig
	PluginsConcerned     []string `json:"plugins_concerned"`
	LatencyThresholdMSec uint32   `json:"latency_threshold_msec"` // up to 4294967295
	BackOffMSec          uint16   `json:"backoff_msec"`           // up to 65535
	AllowTimes           uint32   `json:"allow_times"`            // up to 4294967295
	FlowControlPercentageKey  string   `json:"flow_control_percentage_key"`
}

func latencyLimiterConfigConstructor() plugins.Config {
	return &latencyLimiterConfig{
		LatencyThresholdMSec: 800,
		BackOffMSec:          100,
	}
}

func (c *latencyLimiterConfig) Prepare(pipelineNames []string) error {
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

	if c.LatencyThresholdMSec < 1 {
		return fmt.Errorf("invalid latency millisecond threshold")
	}

	if c.BackOffMSec > 10000 {
		return fmt.Errorf("invalid backoff millisecond (requires less than or equal to 10 seconds)")
	}

	c.FlowControlPercentageKey = strings.TrimSpace(c.FlowControlPercentageKey)

	return nil
}

////

type latencyWindowLimiter struct {
	conf       *latencyLimiterConfig
	instanceId string
}

func latencyLimiterConstructor(conf plugins.Config) (plugins.Plugin, error) {
	c, ok := conf.(*latencyLimiterConfig)
	if !ok {
		return nil, fmt.Errorf("config type want *latencyWindowLimiterConfig got %T", conf)
	}

	l := &latencyWindowLimiter{
		conf: c,
	}
	l.instanceId = fmt.Sprintf("%p", l)

	return l, nil
}

func (l *latencyWindowLimiter) Prepare(ctx pipelines.PipelineContext) {
	registerPluginIndicatorForLimiter(ctx, l.Name(), l.instanceId)
}

func (l *latencyWindowLimiter) Run(ctx pipelines.PipelineContext, t task.Task) (task.Task, error) {
	t.AddFinishedCallback(fmt.Sprintf("%s-checkLatency", l.Name()),
		getTaskFinishedCallbackInLatencyLimiter(ctx, l.conf.PluginsConcerned, l.conf.LatencyThresholdMSec, l.Name()))

	_ = updateInThroughputRate(ctx, l.Name()) // ignore error if it occurs

	counter, err := getLatencyLimiterCounter(ctx, l.Name())
	if err != nil {
		return t, nil // ignore limit if error occurs
	}

	if counter.Count() > uint64(l.conf.AllowTimes) {
		_ = updateFlowControlledThroughputRate(ctx, l.Name())
		if l.conf.BackOffMSec < 1 {
			// service fusing
			t.SetError(fmt.Errorf("service is unavaialbe caused by latency limit"),
				task.ResultFlowControl)
			return t, nil
		}

		select {
		case <-time.After(time.Duration(l.conf.BackOffMSec) * time.Millisecond):
		case <-t.Cancel():
			err := fmt.Errorf("task is cancelled by %s", t.CancelCause())
			t.SetError(err, task.ResultTaskCancelled)
			return t, err
		}
	}

	if len(l.conf.FlowControlPercentageKey) != 0 {
		meter, err := getFlowControlledMeter(ctx, l.Name())
		if err != nil {
			logger.Warnf("[BUG: query flow control percentage data for pipeline %s failed, "+
				"ignored this output]", ctx.PipelineName(), err)
		} else {
			t1, err := task.WithValue(t, l.conf.FlowControlPercentageKey, meter)
			if err != nil {
				t.SetError(err, task.ResultInternalServerError)
				return t, nil
			}
			t = t1
		}
	}
	return t, nil
}

func (l *latencyWindowLimiter) Name() string {
	return l.conf.PluginName()
}

func (h *latencyWindowLimiter) CleanUp(ctx pipelines.PipelineContext) {
	// Nothing to do.
}

func (l *latencyWindowLimiter) Close() {
	// Nothing to do.
}

////

const (
	latencyLimiterCounterKey = "latencyLimiterCounter"
)

type latencyLimiterCounter struct {
	c       chan *bool
	counter uint64
	closed  bool
}

func newLatencyLimiterCounter() *latencyLimiterCounter {
	ret := &latencyLimiterCounter{
		c: make(chan *bool, 32767),
	}

	go func() {
		for {
			select {
			case f := <-ret.c:
				if f == nil {
					return // channel/counter closed, exit
				} else if *f && ret.counter < ^uint64(0) {
					ret.counter += 1
				} else if ret.counter > 0 {
					ret.counter = ret.counter - 1
				}
			}
		}
	}()

	return ret
}

func (c *latencyLimiterCounter) Increase() {
	if !c.closed {
		f := true
		c.c <- &f
	}
}

func (c *latencyLimiterCounter) Decrease() {
	if !c.closed {
		f := false
		c.c <- &f
	}
}

func (c *latencyLimiterCounter) Count() uint64 {
	if c.closed {
		return 0
	}

	for len(c.c) > 0 { // wait counter is updated completely by spin
		time.Sleep(time.Millisecond)
	}

	return c.counter
}

func (c *latencyLimiterCounter) Close() error { // io.Closer stub
	c.closed = true
	close(c.c)
	return nil
}

func getLatencyLimiterCounter(ctx pipelines.PipelineContext, pluginName string) (*latencyLimiterCounter, error) {
	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	counter, err := bucket.QueryDataWithBindDefault(latencyLimiterCounterKey,
		func() interface{} {
			return newLatencyLimiterCounter()
		})
	if err != nil {
		logger.Warnf("[BUG: query state data for pipeline %s failed, "+
			"ignored to limit request: %v]", ctx.PipelineName(), err)
		return nil, err
	}

	return counter.(*latencyLimiterCounter), nil
}

func getTaskFinishedCallbackInLatencyLimiter(ctx pipelines.PipelineContext, pluginsConcerned []string,
	latencyThresholdMSec uint32, pluginName string) task.TaskFinished {

	return func(t1 task.Task, _ task.TaskStatus) {
		t1.DeleteFinishedCallback(fmt.Sprintf("%s-checkLatency", pluginName))

		kind := pipelines.SuccessStatistics
		if !task.SuccessfulResult(t1.ResultCode()) {
			kind = pipelines.FailureStatistics
		}

		var latency float64
		var found bool

		for _, name := range pluginsConcerned {
			if !common.StrInSlice(name, ctx.PluginNames()) {
				continue // ignore safely
			}

			rt, err := ctx.Statistics().PluginExecutionTimePercentile(
				name, kind, 0.9) // value 90% is an option?
			if err != nil {
				logger.Warnf("[BUG: query plugin %s 90%% execution time failed, "+
					"ignored to adjust exceptional latency counter: %v]", pluginName, err)
				return
			}
			if rt < 0 {
				continue // doesn't make sense, defensive
			}

			latency += rt
			found = true
		}

		if !found {
			return
		}

		counter, err := getLatencyLimiterCounter(ctx, pluginName)
		if err != nil {
			return
		}

		if latency < float64(time.Duration(latencyThresholdMSec)*time.Millisecond) {
			counter.Decrease()
		} else {
			counter.Increase()
		}
	}
}

///

const limiterFlowControlledThroughputRate1Key = "LimiterFlowControlledRateKey"
const limiterInKeyThroughputRate1 = "LimiterInRateKey"

func getInThroughputRate1(ctx pipelines.PipelineContext, pluginName string) (*common.ThroughputStatistic, error) {
	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	rate, err := bucket.QueryDataWithBindDefault(limiterInKeyThroughputRate1, func() interface{} {
		return common.NewThroughputStatistic(common.ThroughputRate1)
	})
	if err != nil {
		logger.Warnf("[BUG: query in-throughput rate data for pipeline %s failed, "+
			"ignored statistic: %v]", ctx.PipelineName(), err)
		return nil, err
	}

	return rate.(*common.ThroughputStatistic), nil
}

func getFlowControlledThroughputRate1(ctx pipelines.PipelineContext, pluginName string) (*common.ThroughputStatistic, error) {
	bucket := ctx.DataBucket(pluginName, pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE)
	rate, err := bucket.QueryDataWithBindDefault(limiterFlowControlledThroughputRate1Key, func() interface{} {
		return common.NewThroughputStatistic(common.ThroughputRate1)
	})
	if err != nil {
		logger.Warnf("[BUG: query flow controlled throughput rate data for pipeline %s failed, "+
			"ignored statistic: %v]", ctx.PipelineName(), err)
		return nil, err
	}

	return rate.(*common.ThroughputStatistic), nil
}

func getFlowControlledMeter(ctx pipelines.PipelineContext, pluginName string) (int, error) {
	rate, err := getFlowControlledThroughputRate1(ctx, pluginName)
	if err != nil {
		return 0, err
	}
	pluginFlowControlledRate, err := rate.Get()
	if err != nil {
		return 0, err
	}
	rate, err = getInThroughputRate1(ctx, pluginName)
	if err != nil {
		return 0, err
	}
	pluginInThroughputRate, err := rate.Get()
	if pluginInThroughputRate == 0 { // avoid divide by zero
		return 0, nil
	}
	return int(100.0 * pluginFlowControlledRate / pluginInThroughputRate), nil
}

func registerPluginIndicatorForLimiter(ctx pipelines.PipelineContext, pluginName, pluginInstanceId string) {
	_, err := ctx.Statistics().RegisterPluginIndicator(pluginName, pluginInstanceId, "THROUGHPUT_RATE_LAST_1MIN_FLOWCONTROLLED", "Flow controlled throughput rate of the plugin in last 1 minute.", func(pluginName, indicatorName string) (interface{}, error) {
		rate, err := getFlowControlledThroughputRate1(ctx, pluginName)
		if err != nil {
			return nil, err
		}
		return rate.Get()
	})
	if err != nil {
		logger.Warnf("[BUG: register plugin indicator for pipeline %s plugin %s failed: %v]", ctx.PipelineName(), pluginName, err)
	}

	// We don't use limiter plugin's THROUGHPUT_RATE_LAST_1MIN_ALL because it indicates the throughput rate after applying flow control
	_, err = ctx.Statistics().RegisterPluginIndicator(pluginName, pluginInstanceId, "THROUGHPUT_RATE_LAST_1MIN_IN", "in(not flow controled) throughput rate of the plugin in last 1 minute.", func(pluginName, indicatorName string) (interface{}, error) {
		rate, err := getInThroughputRate1(ctx, pluginName)
		if err != nil {
			return nil, err
		}
		return rate.Get()
	})
	if err != nil {
		logger.Warnf("[BUG: register plugin indicator for pipeline %s plugin %s failed: %v]", ctx.PipelineName(), pluginName, err)
	}
}

func updateFlowControlledThroughputRate(ctx pipelines.PipelineContext, pluginName string) error {
	flowControlledRate1, err := getFlowControlledThroughputRate1(ctx, pluginName)
	if err != nil {
		return err
	}
	flowControlledRate1.Update(1)
	return nil
}

func updateInThroughputRate(ctx pipelines.PipelineContext, pluginName string) error {
	inRate1, err := getInThroughputRate1(ctx, pluginName)
	if err != nil {
		return err
	}
	inRate1.Update(1)
	return nil
}