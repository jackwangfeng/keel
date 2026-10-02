package dtm

// Coordinator 是业务代码看得到的协调器能力：提交 SAGA、等终态、二阶段消息的三步、查状态。
//
// 两个实现，按部署形态二选一（docs/电商系统-微服务部署方案.md）：
//
//	· *TC（嵌入式，FFI）：单体形态。协调器跑在本进程里，分支可以是 local:// 函数。
//	· *Remote（独立部署的 dtmrs 集群）：微服务形态。本进程只是客户端，所有分支都是 HTTP 地址，
//	  协调器经内网回调各服务的 /internal/v1/saga/<名字>。
//
// 两者对同一组调用给出同样的语义（状态串、错误的含义），业务代码不知道背后是哪一种。
type Coordinator interface {
	SubmitSaga(gid, stepsJSON string) error
	SubmitSagaSteps(gid string, steps ...Step) error
	WaitFinal(gid string, timeoutMS int) (string, error)
	PrepareMsg(gid string, actions []string, queryPrepared string, graceSecs int) error
	PrepareMsgEx(gid string, actions, payloads []string, queryPrepared string, graceSecs int, allowEmptyTopic bool) error
	SubmitMsg(gid string) error
	AbortMsg(gid string) error
	Status(gid string) (string, error)
	Close()
}

var (
	_ Coordinator = (*TC)(nil)
	_ Coordinator = (*Remote)(nil)
)

// TopicPrefix 是按主题投递的地址前缀（dtmrs 0.12，与 DTM 同协议）：action 写成 topic://<名字>，
// 协调器在 prepare 那一刻按订阅关系展开成具体分支。
const TopicPrefix = "topic://"
