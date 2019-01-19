package teaagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/TeaWeb/agent/teaconfigs"
	"github.com/TeaWeb/code/teaconfigs/agents"
	"github.com/go-yaml/yaml"
	"github.com/iwind/TeaGo/Tea"
	"github.com/iwind/TeaGo/files"
	"github.com/iwind/TeaGo/lists"
	"github.com/iwind/TeaGo/logs"
	"github.com/iwind/TeaGo/maps"
	"github.com/iwind/TeaGo/types"
	"github.com/syndtr/goleveldb/leveldb"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sync"
	"time"
)

var connectConfig *teaconfigs.AgentConfig = nil
var runningAgent *agents.AgentConfig = nil
var runningTasks = map[string]*Task{} // task id => task
var runningTasksLocker = sync.Mutex{}
var runningItems = map[string]*Item{} // item id => task
var runningItemsLocker = sync.Mutex{}
var isBooting = true

// 启动
func Start() {
	// 连接配置
	{
		config, err := teaconfigs.SharedAgentConfig()
		if err != nil {
			logs.Println("start failed:" + err.Error())
			return
		}
		connectConfig = config
	}

	// 启动
	if lists.Contains(os.Args, "start") {
		onStart()
		return
	}

	// 停止
	if lists.Contains(os.Args, "stop") {
		onStop()
		return
	}

	// 重启
	if lists.Contains(os.Args, "restart") {
		onStop()
		onStart()
		return
	}

	// 运行某个脚本
	if lists.Contains(os.Args, "run") {
		if len(os.Args) <= 2 {
			logs.Println("no task to run")
			return
		}

		taskId := os.Args[2]
		if len(taskId) == 0 {
			logs.Println("no task to run")
			return
		}

		agent := agents.NewAgentConfigFromId(connectConfig.Id)
		if agent == nil {
			logs.Println("agent not found")
			return
		}
		appConfig, taskConfig := agent.FindTask(taskId)
		if taskConfig == nil {
			logs.Println("task not found")
			return
		}

		task := NewTask(appConfig.Id, taskConfig)
		_, stdout, stderr, err := task.Run()
		if len(stdout) > 0 {
			log.Print("stdout:", stdout)
		}
		if len(stderr) > 0 {
			log.Print("stderr:", stderr)
		}
		if err != nil {
			log.Print(err.Error())
		}

		return
	}

	// 帮助
	if lists.ContainsAny(os.Args, "h", "-h", "help", "-help") {
		fmt.Print(`Usage:
~~~
bin/teaweb-agent						
   run in foreground

bin/teaweb-agent help 					
   show help

bin/teaweb-agent start 					
   start agent

bin/teaweb-agent stop 					
   stop agent

bin/teaweb-agent restart				
   restart agent

bin/teaweb-agent run [TASK ID]			
   run task
~~~
`)
		return
	}

	// 测试连接
	if lists.Contains(os.Args, "test") {
		err := testConnection()
		if err != nil {
			logs.Println("error:", err.Error())
		} else {
			logs.Println("connection to master is ok")
		}
		return
	}

	// 日志
	if lists.Contains(os.Args, "background") {
		logDir := files.NewFile(Tea.Root + "/logs")
		if !logDir.IsDir() {
			logDir.Mkdir()
		}

		fp, err := os.OpenFile(Tea.Root+"/logs/run.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err == nil {
			log.SetOutput(fp)
		} else {
			log.Println(err)
		}
	}

	logs.Println("starting ...")

	// 下载配置
	{
		err := downloadConfig()
		if err != nil {
			logs.Println("start failed:" + err.Error())
			return
		}
	}

	// 启动
	bootTasks()
	isBooting = false

	// 定时
	scheduleTasks()

	// 监控项数据
	scheduleItems()

	// 推送日志
	go pushEvents()

	// 同步配置
	for {
		err := pullEvents()
		if err != nil {
			logs.Println("pull error:", err.Error())
			time.Sleep(5 * time.Second)
		}
	}
}

// 下载配置
func downloadConfig() error {
	// 本地
	if connectConfig.Id == "local" {
		loadLocalConfig()
		return nil
	}

	// 远程的
	master := connectConfig.Master
	if len(master) == 0 {
		return errors.New("'master' should not be empty")
	}
	req, err := http.NewRequest(http.MethodGet, master+"/api/agent", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "TeaWeb Agent")
	req.Header.Set("Tea-Agent-Id", connectConfig.Id)
	req.Header.Set("Tea-Agent-Key", connectConfig.Key)
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New("invalid status response from master '" + fmt.Sprintf("%d", resp.StatusCode) + "'")
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	respMap := maps.Map{}
	err = json.Unmarshal(data, &respMap)
	if err != nil {
		return err
	}

	if respMap == nil {
		return errors.New("response data should not be nil")
	}

	if respMap.GetInt("code") != 200 {
		return errors.New("invalid response from master:" + string(data))
	}

	jsonData := respMap.Get("data")
	if jsonData == nil || reflect.TypeOf(jsonData).Kind() != reflect.Map {
		return errors.New("response json data should be a map")
	}

	dataMap := maps.NewMap(jsonData)
	config := dataMap.GetString("config")

	agent := &agents.AgentConfig{}
	err = yaml.Unmarshal([]byte(config), agent)
	if err != nil {
		return err
	}

	if len(agent.Id) == 0 {
		return errors.New("invalid agent id")
	}

	err = agent.Validate()
	if err != nil {
		return err
	}

	// 保存
	agentsDir := files.NewFile(Tea.ConfigFile("agents/"))
	if !agentsDir.IsDir() {
		err = agentsDir.Mkdir()
		if err != nil {
			return err
		}
	}
	agentFile := files.NewFile(Tea.ConfigFile("agents/agent." + agent.Id + ".conf"))
	err = agentFile.WriteString(config)
	if err != nil {
		return err
	}

	runningAgent = agent
	connectConfig.Id = agent.Id
	connectConfig.Key = agent.Key

	if !isBooting {
		// 定时任务
		scheduleTasks()

		// 监控项数据
		scheduleItems()
	}

	return nil
}

// 加载Local配置
func loadLocalConfig() error {
	agent := agents.NewAgentConfigFromFile("agent.local.conf")
	if agent == nil {
		time.Sleep(30 * time.Second)
		return loadLocalConfig()
	}
	err := agent.Validate()
	if err != nil {
		logs.Println("[agent]" + err.Error())
		time.Sleep(30 * time.Second)
		return loadLocalConfig()
	}
	runningAgent = agent
	connectConfig.Key = agent.Key

	if !isBooting {
		return scheduleTasks()
	}
	return nil
}

// 启动任务
func bootTasks() {
	logs.Println("booting ...")
	if !runningAgent.On {
		return
	}
	for _, app := range runningAgent.Apps {
		if !app.On {
			continue
		}
		for _, taskConfig := range app.Tasks {
			if !taskConfig.On {
				continue
			}
			task := NewTask(app.Id, taskConfig)
			if task.ShouldBoot() {
				err := task.RunLog()
				if err != nil {
					logs.Println(err.Error())
				}
			}
		}
	}
}

// 定时任务
func scheduleTasks() error {
	// 生成脚本
	taskIds := []string{}

	for _, app := range runningAgent.Apps {
		if !app.On {
			continue
		}
		for _, taskConfig := range app.Tasks {
			if !taskConfig.On {
				continue
			}
			taskIds = append(taskIds, taskConfig.Id)

			// 是否正在运行
			runningTask, found := runningTasks[taskConfig.Id]
			isChanged := true
			if found {
				// 如果有修改，则需要重启
				if runningTask.config.Version != taskConfig.Version {
					logs.Println("stop schedule task", taskConfig.Id, taskConfig.Name)
					runningTask.Stop()

					if taskConfig.On && len(taskConfig.Schedule) > 0 {
						logs.Println("restart schedule task", taskConfig.Id, taskConfig.Name)
						runningTask.config = taskConfig
						runningTask.Schedule()
					}
				} else {
					isChanged = false
				}
			} else if taskConfig.On && len(taskConfig.Schedule) > 0 { // 新任务，则启动
				logs.Println("schedule task", taskConfig.Id, taskConfig.Name)
				task := NewTask(app.Id, taskConfig)
				task.Schedule()

				runningTasksLocker.Lock()
				runningTasks[taskConfig.Id] = task
				runningTasksLocker.Unlock()
			}

			// 生成脚本
			if isChanged {
				_, err := taskConfig.GenerateAgain()
				if err != nil {
					return err
				}
			}
		}
	}

	// 停止运行
	for taskId, runningTask := range runningTasks {
		if !lists.Contains(taskIds, taskId) {
			runningTasksLocker.Lock()
			delete(runningTasks, taskId)
			runningTasksLocker.Unlock()
			err := runningTask.Stop()
			if err != nil {
				logs.Error(err)
			}
		}
	}

	// 删除不存在的任务脚本
	files.NewFile(Tea.ConfigFile("agents/")).Range(func(file *files.File) {
		filename := file.Name()
		if !regexp.MustCompile("^task\\.\\w+\\.script$").MatchString(filename) {
			return
		}
		taskId := filename[len("task:") : len(filename)-len(".script")]
		if !lists.Contains(taskIds, taskId) {
			err := file.Delete()
			if err != nil {
				logs.Error(err)
			}
		}
	})

	return nil
}

// 监控数据采集
func scheduleItems() error {
	logs.Println("schedule items")
	itemIds := []string{}

	for _, app := range runningAgent.Apps {
		if !app.On {
			continue
		}
		for _, itemConfig := range app.Items {
			if !itemConfig.On {
				continue
			}
			runningItemsLocker.Lock()
			itemIds = append(itemIds, itemConfig.Id)
			runningItem, found := runningItems[itemConfig.Id]
			if found {
				runningItem.Stop()
			}

			item := NewItem(app.Id, itemConfig)
			item.Schedule()
			runningItems[itemConfig.Id] = item
			logs.Println("add item", item.config.Name)
			runningItemsLocker.Unlock()
		}
	}

	// 删除不运行的
	for itemId, item := range runningItems {
		if !lists.Contains(itemIds, itemId) {
			item.Stop()
			runningItemsLocker.Lock()
			delete(runningItems, itemId)
			logs.Println("delete item", item.config.Name)
			runningItemsLocker.Unlock()
		}
	}

	return nil
}

// 从主服务器同步数据
func pullEvents() error {
	//logs.Println("pull events ...")
	master := connectConfig.Master
	if len(master) == 0 {
		return errors.New("'master' should not be empty")
	}
	req, err := http.NewRequest(http.MethodGet, master+"/api/agent/pull", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "TeaWeb Agent")
	req.Header.Set("Tea-Agent-Id", connectConfig.Id)
	req.Header.Set("Tea-Agent-Key", connectConfig.Key)
	connectingFailed := false
	client := http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// 握手配置
				conn, err := (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 0,
					DualStack: true,
				}).DialContext(ctx, network, addr)
				if err != nil {
					connectingFailed = true
				}
				return conn, err
			},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if connectingFailed {
			return err
		}

		// 如果是超时的则不提示，因为长连接依赖超时设置
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New("invalid status response from master '" + fmt.Sprintf("%d", resp.StatusCode) + "'")
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	respMap := maps.Map{}
	err = json.Unmarshal(data, &respMap)
	if err != nil {
		return err
	}

	if respMap == nil {
		return errors.New("response data should not be nil")
	}

	if respMap.GetInt("code") != 200 {
		return errors.New("invalid response from master:" + string(data))
	}

	jsonData := respMap.Get("data")
	if jsonData == nil || reflect.TypeOf(jsonData).Kind() != reflect.Map {
		return errors.New("response json data should be a map")
	}

	dataMap := maps.NewMap(jsonData)
	events := dataMap.Get("events")
	if events == nil || reflect.TypeOf(events).Kind() != reflect.Slice {
		return nil
	}

	eventsValue := reflect.ValueOf(events)
	count := eventsValue.Len()
	for i := 0; i < count; i ++ {
		event := eventsValue.Index(i).Interface()
		if event == nil || reflect.TypeOf(event).Kind() != reflect.Map {
			continue
		}
		eventMap := maps.NewMap(event)
		name := eventMap.GetString("name")
		switch name {
		case "UPDATE_AGENT":
			downloadConfig()
		case "ADD_APP":
			downloadConfig()
		case "UPDATE_APP":
			downloadConfig()
		case "REMOVE_APP":
			downloadConfig()
		case "ADD_TASK":
			downloadConfig()
		case "UPDATE_TASK":
			downloadConfig()
		case "REMOVE_TASK":
			downloadConfig()
		case "RUN_TASK":
			eventDataMap := eventMap.GetMap("data")
			if eventDataMap != nil {
				taskId := eventDataMap.GetString("taskId")
				appConfig, taskConfig := runningAgent.FindTask(taskId)
				if taskConfig == nil {
					logs.Println("error:no task with id '" + taskId + "found")
				} else {
					task := NewTask(appConfig.Id, taskConfig)
					task.RunLog()
				}
			} else {
				logs.Println("invalid event data: should be a map")
			}
		case "ADD_ITEM":
			downloadConfig()
		case "UPDATE_ITEM":
			downloadConfig()
		case "DELETE_ITEM":
			downloadConfig()
		}
	}

	return nil
}

// 向Master同步事件
func pushEvents() {
	db, err := leveldb.OpenFile(Tea.Root+"/logs/leveldb", nil)
	if err != nil {
		logs.Println("error:", err.Error())
	}
	defer db.Close()

	// 读取本地数据库日志并发送到Master
	go func() {
		for {
			if db == nil {
				break
			}
			iterator := db.NewIterator(nil, nil)
			for iterator.Next() {
				value := iterator.Value()
				req, err := http.NewRequest(http.MethodPut, connectConfig.Master+"/api/agent/push", bytes.NewReader(value))
				if err != nil {
					logs.Println("error:", err.Error())
					time.Sleep(5 * time.Second)
				} else {
					func() {
						req.Header.Set("User-Agent", "TeaWeb Agent")
						req.Header.Set("Tea-Agent-Id", connectConfig.Id)
						req.Header.Set("Tea-Agent-Key", connectConfig.Key)
						client := http.Client{
							Timeout: 5 * time.Second,
						}
						resp, err := client.Do(req)

						if err != nil {
							logs.Println("error:", err.Error())
							return
						}
						defer resp.Body.Close()
						if resp.StatusCode != 200 {
							return
						}

						respBody, err := ioutil.ReadAll(resp.Body)
						if err != nil {
							logs.Println("error:", err.Error())
							return
						}

						respJSON := maps.Map{}
						err = json.Unmarshal(respBody, &respJSON)
						if err != nil {
							logs.Println("error:", err.Error())
							return
						}

						if respJSON.GetInt("code") != 200 {
							logs.Println("[/api/agent/push]error response from master:", string(respBody))
							// 防止不停地提交请求
							time.Sleep(5 * time.Second)
							return
						}
						db.Delete(iterator.Key(), nil)
					}()
				}
			}
			time.Sleep(1 * time.Second)
		}
	}()

	// 读取日志并写入到本地数据库
	logId := time.Now().Unix()
	for {
		event := <-eventQueue

		if runningAgent.Id != "local" {
			// 进程事件
			if event, found := event.(*ProcessEvent); found {
				if event.EventType == ProcessEventStdout || event.EventType == ProcessEventStderr {
					log.Print("[" + findTaskName(event.TaskId) + "]" + string(event.Data))
				} else if event.EventType == ProcessEventStart {
					log.Print("[" + findTaskName(event.TaskId) + "]start")
				} else if event.EventType == ProcessEventStop {
					log.Print("[" + findTaskName(event.TaskId) + "]stop")
				}
			}
		}

		jsonData, err := event.AsJSON()
		if err != nil {
			logs.Println("error:", err.Error())
			continue
		}

		if db != nil {
			logId ++
			db.Put([]byte(fmt.Sprintf("%d", logId)), jsonData, nil)
		}
	}
}

// 启动
func onStart() {
	cmdFile := os.Args[0]
	cmd := exec.Command(cmdFile, "background")
	cmd.Dir = Tea.Root
	err := cmd.Start()
	if err != nil {
		logs.Error(err)
		return
	}

	failed := false
	go func() {
		err = cmd.Wait()
		if err != nil {
			logs.Error(err)
		}

		failed = true
	}()

	time.Sleep(1 * time.Second)
	if failed {
		logs.Println("error: process terminated, lookup 'logs/run.log' for more details")
	} else {
		// write pid
		pidFile := files.NewFile(Tea.Root + "/logs/pid")
		pidFile.WriteString(fmt.Sprintf("%d", cmd.Process.Pid))

		logs.Println("start success, pid:" + fmt.Sprintf("%d", cmd.Process.Pid))
	}
}

// 停止
func onStop() {
	pidFile := files.NewFile(Tea.Root + "/logs/pid")
	pid, err := pidFile.ReadAllString()
	if err != nil {
		logs.Println("error:", err.Error())
	} else {
		process, err := os.FindProcess(types.Int(pid))
		if err != nil {
			logs.Println("error:", err.Error())
		} else {
			process.Kill()
			logs.Println("stopped pid", pid)
		}
	}
}

// 测试连接
func testConnection() error {
	master := connectConfig.Master
	if len(master) == 0 {
		return errors.New("'master' should not be empty")
	}
	req, err := http.NewRequest(http.MethodGet, master+"/api/agent", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "TeaWeb Agent")
	req.Header.Set("Tea-Agent-Id", connectConfig.Id)
	req.Header.Set("Tea-Agent-Key", connectConfig.Key)
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New("invalid status response from master '" + fmt.Sprintf("%d", resp.StatusCode) + "'")
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	respMap := maps.Map{}
	err = json.Unmarshal(data, &respMap)
	if err != nil {
		return err
	}

	if respMap == nil {
		return errors.New("response data should not be nil")
	}

	if respMap.GetInt("code") != 200 {
		return errors.New("invalid response from master:" + string(data))
	}

	jsonData := respMap.Get("data")
	if jsonData == nil || reflect.TypeOf(jsonData).Kind() != reflect.Map {
		return errors.New("response json data should be a map")
	}

	dataMap := maps.NewMap(jsonData)
	config := dataMap.GetString("config")

	agent := &agents.AgentConfig{}
	err = yaml.Unmarshal([]byte(config), agent)
	if err != nil {
		return err
	}

	if len(agent.Id) == 0 {
		return errors.New("invalid agent id")
	}

	err = agent.Validate()
	if err != nil {
		return err
	}

	return nil
}

// 查找任务
func findTaskName(taskId string) string {
	if runningAgent == nil {
		return ""
	}
	_, task := runningAgent.FindTask(taskId)
	if task == nil {
		return ""
	}
	return task.Name
}
