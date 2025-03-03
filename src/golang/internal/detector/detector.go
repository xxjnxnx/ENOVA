package detector

import (
	"encoding/json"
	"enova/internal/meta"
	"enova/internal/resource"
	"enova/pkg/api"
	"enova/pkg/config"
	"enova/pkg/logger"
	"enova/pkg/redis"
	"enova/pkg/zmq"
	"fmt"
	"time"
)

type DetectResultManager struct {
	RedisClient *redis.RedisClient
}

func (t *DetectResultManager) AppendAnomalyResult(task meta.TaskSpec, result meta.AnomalyRecommendResult) error {
	// 使用LPUSH将新元素插入列表的左侧
	taskRecommentResultKey := fmt.Sprintf("%s_recomment_results", task.Name)
	resultBytes, _ := json.Marshal(result)
	if err := t.RedisClient.AppendListWithLimitSize(taskRecommentResultKey, string(resultBytes), 10); err != nil {
		return err
	}
	return nil
}

func (t *DetectResultManager) GetHistoricalAnomalyRecommendResult(task meta.TaskSpec) []meta.AnomalyRecommendResult {
	// 使用LPUSH将新元素插入列表的左侧
	ret := []meta.AnomalyRecommendResult{}
	taskRecommentResultKey := fmt.Sprintf("%s_recomment_results", task.Name)
	jsonArray, err := t.RedisClient.GetList(taskRecommentResultKey)
	if err != nil {
		return ret
	}

	for _, jsonItem := range jsonArray {
		var result meta.AnomalyRecommendResult
		if err := json.Unmarshal([]byte(jsonItem), &result); err != nil {
			logger.Errorf("GetHistoricalAnomalyRecommendResult Unmarshal json err: %v, jsonItem: %s ", err, jsonItem)
			continue
		}
		ret = append(ret, result)
	}
	return ret
}

type Detector struct {
	Publisher           zmq.ZmqPublisher
	PermCli             PerformanceDetectorCli
	Client              resource.ClientInterface
	TaskMap             map[string]*meta.DetectTask
	DetectResultManager *DetectResultManager
}

func NewDetector() *Detector {
	pub := zmq.ZmqPublisher{
		Host: config.GetEConfig().Zmq.Host,
		Port: config.GetEConfig().Zmq.Port,
	}
	pub.Init()
	return &Detector{
		Publisher: pub,
		PermCli:   PerformanceDetectorCli{},
		TaskMap:   make(map[string]*meta.DetectTask),
		Client:    resource.NewDockerResourcClient(),
		DetectResultManager: &DetectResultManager{
			RedisClient: redis.NewRedisClient(),
		},
	}
}

func (d *Detector) SendScaleTask(task meta.TaskSpecInterface) {
	scaleTaskJson, err := json.Marshal(task)
	if err != nil {
		logger.Errorf("DetectOnce json Marshal err: %v", err)
		return
	}

	if ok, err := d.Publisher.Send(string(scaleTaskJson)); err != nil {
		logger.Errorf("DetectOnce Publisher Send err: %v, ok: %v", err, ok)
		return
	}
}

func (d *Detector) AnomalyDetect(spec meta.TaskSpecInterface) (bool, error) {
	requestParams, err := d.PermCli.GetVllmCurrentMetricParams(spec)
	if err != nil {
		return false, err
	}
	params := api.AnomalyDetectRequest{
		Metrics:        requestParams.Metrics,
		Configurations: requestParams.Configurations,
	}
	headers := make(map[string]string)
	enovaResp, err := api.GetEnovaAlgoClient().AnomalyDetect.Call(params, headers)
	if err != nil {
		return false, err
	}

	resultData, err := json.Marshal(enovaResp.Result)
	if err != nil {
		logger.Errorf("encode resp.Result err: %v", err)
		return false, err
	}

	var anomalyDetectResp api.AnomalyDetectResponse
	if err := json.Unmarshal(resultData, &anomalyDetectResp); err != nil {
		logger.Errorf("encode ConfigRecommendResult err: %v", err)
		return false, err
	}
	return anomalyDetectResp.IsAnomaly > 0, err
}

// DetectOneTaskSpec first Check anomaly detection, then get anomaly recovery
func (d *Detector) DetectOneTaskSpec(taskName string, taskSpec meta.TaskSpecInterface) {
	anomalyResult, err := d.AnomalyDetect(taskSpec)
	if err != nil {
		logger.Errorf("DetectOneTaskSpec AnomalyDetect get error: %v", err)
	} else {
		logger.Infof("DetectOneTaskSpec AnomalyDetect result: %v", anomalyResult)
	}
	if anomalyResult {
		requestParams, err := d.PermCli.GetVllmCurrentMetricParams(taskSpec)
		if err != nil {
			logger.Errorf("DetectOnce err: %v", err)
			return
		}
		headers := make(map[string]string)
		resp, err := api.GetEnovaAlgoClient().AnomalyRecover.Call(requestParams, headers)
		if err != nil {
			logger.Errorf("AnomalyRecover err: %v", err)
			return
		}

		resultData, err := json.Marshal(resp.Result)
		if err != nil {
			logger.Errorf("DetectOneTaskSpec encode resp.Result err: %v", err)
			return
		}

		var result api.ConfigRecommendResult
		if err := json.Unmarshal(resultData, &result); err != nil {
			logger.Errorf("DetectOneTaskSpec encode ConfigRecommendResult err: %v", err)
			return
		}

		// TODO: adapt more config
		currentConfig, ok := taskSpec.GetBackendConfig().(*meta.VllmBackendConfig)
		if !ok {
			logger.Errorf("DetectOneTaskSpec Get VllmBackendConfig failed")
			return
		}
		d.UpdateTaskSpec(taskSpec, result)
		d.SendScaleTask(taskSpec)
		d.DetectResultManager.AppendAnomalyResult(*taskSpec.(*meta.TaskSpec), meta.AnomalyRecommendResult{
			Timestamp:             time.Now().UnixMilli(),
			IsAnomaly:             anomalyResult,
			ConfigRecommendResult: result,
			CurrentConfig: api.ConfigRecommendResult{
				MaxNumSeqs:           currentConfig.MaxNumSeqs,
				TensorParallelSize:   currentConfig.TensorParallelSize,
				GpuMemoryUtilization: currentConfig.GpuMemoryUtilization,
				Replicas:             taskSpec.GetReplica(),
			},
		})
	}
}

// DetectOnce Detect anomaly from remote
func (d *Detector) DetectOnce() {
	logger.Infof("DetectOnce start detect once")
	for taskName, task := range d.TaskMap {
		if d.IsTaskRunning(taskName, task.TaskSpec) {
			d.DetectOneTaskSpec(taskName, task.TaskSpec)
		}
	}
}

// IsTaskRunning TODO: add GetTaskMap
func (d *Detector) IsTaskRunning(taskName string, task meta.TaskSpecInterface) bool {
	t := task.(*meta.TaskSpec)
	if d.Client.IsTaskRunning(*t) {
		d.TaskMap[taskName].Status = meta.TaskStatusRunning
		return true
	}
	d.TaskMap[taskName].Status = meta.TaskStatusScheduling
	return false
}

// UpdateTaskSpec
func (d *Detector) UpdateTaskSpec(task meta.TaskSpecInterface, resp api.ConfigRecommendResult) {
	task.UpdateBackendConfig(resp)
}

func (d *Detector) RunDetector() {
	ticker := time.NewTicker(time.Duration(time.Duration(config.GetEConfig().Detector.DetectInterval) * time.Second))
	for {
		select {
		case <-ticker.C:
			d.DetectOnce()
		}
	}
}

// RegisterTask Register Task to taskmap
func (d *Detector) RegisterTask(task meta.TaskSpec) {
	if err := d.UpdateEnodeInitialBackendConfigByRemote(&task); err != nil {
		logger.Errorf("UpdateEnodeInitialBackendConfigByRemote err: %v", err)
		return
	}

	d.SendScaleTask(&task)
	// register at last
	d.TaskMap[task.Name] = &meta.DetectTask{
		TaskSpec: &task,
		Status:   meta.TaskStatusCreated,
	}
}

func (d *Detector) DeleteTask(taskName string) {
	task, ok := d.TaskMap[taskName]
	if !ok {
		logger.Infof("taskName: %s is not register", taskName)
		return
	}
	delete(d.TaskMap, taskName)
	task.TaskSpec.UpdateReplica(0)
	d.SendScaleTask(task.TaskSpec)
}

// UpdateEnodeInitialBackendConfigByRemote Get Remote recommending backendCOnfig When first deploy,
func (d *Detector) UpdateEnodeInitialBackendConfigByRemote(spec meta.TaskSpecInterface) error {
	params := api.ConfigRecommendRequest{
		Llm: spec.GetModelConfig().Llm,
		Gpu: spec.GetModelConfig().Gpu,
	}
	headers := make(map[string]string)
	resp, err := api.GetEnovaAlgoClient().ConfigRecommend.Call(params, headers)
	if err != nil {
		return err
	}

	resultData, err := json.Marshal(resp.Result)
	if err != nil {
		logger.Errorf("encode resp.Result err: %v", err)
		return err
	}

	var result api.ConfigRecommendResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		logger.Errorf("encode ConfigRecommendResult err: %v", err)
		return err
	}
	spec.UpdateBackendConfig(result)
	return nil
}
