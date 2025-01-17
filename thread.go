package thread

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/gospider007/chanx"
)

type Client struct {
	debug             bool                                      //是否显示调试信息
	createThreadValue func(context.Context, int64) (any, error) //每一个线程开始时，根据线程id,创建一个局部对象
	clearThreadValue  func(context.Context, any) error          //线程被消毁时的回调,再这里可以安全的释放局部对象资源
	taskDoneCallBack  func(*Task) error                         //任务回调

	ctx2         context.Context      //控制各个协程
	cnl2         context.CancelFunc   //控制各个协程
	ctx          context.Context      //控制主进程，不会关闭各个协程
	cnl          context.CancelFunc   //控制主进程，不会关闭各个协程
	ctx3         context.Context      //chanx 的协程控制
	cnl3         context.CancelFunc   //chanx 的协程控制
	tasks        chan *Task           //任务消费队列
	threadTokens chan struct{}        //线程可用队列
	dones        chan struct{}        //任务完成通知队列
	tasks2       *chanx.Client[*Task] //chanx 的队列任务
	err          error
	maxThreadId  atomic.Int64
	maxNum       int

	runAfterTime *time.Timer
}

type Task struct {
	Func    any           //运行的函数
	Args    []any         //传入的参数
	Timeout time.Duration //超时时间
	err     error         //函数错误信息
	result  []any         //函数执行的结果
	ctx     context.Context
	cnl     context.CancelFunc
	stat    uint8
}

func (obj *Task) Result() ([]any, error) {
	return obj.result, obj.Error()
}

func (obj *Task) Error() error {
	if obj.err != nil || obj.stat == 6 {
		return obj.err
	}
	switch obj.stat {
	case 0:
		return errors.New("task init error")
	case 1:
		return errors.New("task start error")
	case 2:
		return errors.New("task ctx error")
	case 3:
		return errors.New("task params error")
	case 4:
		return errors.New("task func error")
	case 5:
		return errors.New("task callback error")
	}
	return obj.err
}
func (obj *Task) Done() <-chan struct{} {
	return obj.ctx.Done()
}

type ClientOption struct {
	Debug             bool                                      //是否显示调试信息
	CreateThreadValue func(context.Context, int64) (any, error) //每一个线程开始时，根据线程id,创建一个局部对象
	ClearThreadValue  func(context.Context, any) error          //线程被消毁时的回调,再这里可以安全的释放局部对象资源
	TaskDoneCallBack  func(*Task) error                         //有序的任务完成回调
}

func NewClient(preCtx context.Context, maxNum int, options ...ClientOption) *Client {
	if preCtx == nil {
		preCtx = context.TODO()
	}
	if maxNum < 1 {
		maxNum = 1
	}
	var option ClientOption
	if len(options) > 0 {
		option = options[0]
	}
	ctx, cnl := context.WithCancel(preCtx)
	ctx2, cnl2 := context.WithCancel(preCtx)

	tasks := make(chan *Task)
	dones := make(chan struct{}, 1)

	threadTokens := make(chan struct{}, maxNum)
	for i := 0; i < int(maxNum); i++ {
		threadTokens <- struct{}{}
	}
	pool := &Client{
		debug:             option.Debug,             //是否显示调试信息
		createThreadValue: option.CreateThreadValue, //每一个线程开始时，根据线程id,创建一个局部对象
		clearThreadValue:  option.ClearThreadValue,  //线程被消毁时的回调,再这里可以安全的释放局部对象资源
		taskDoneCallBack:  option.TaskDoneCallBack,  //任务回调

		maxNum:       maxNum,
		ctx2:         ctx2,
		cnl2:         cnl2, //关闭协程
		ctx:          ctx,
		cnl:          cnl,   //通知关闭
		tasks:        tasks, //任务队列
		threadTokens: threadTokens,
		dones:        dones,
	}
	if option.TaskDoneCallBack != nil { //任务完成回调
		pool.tasks2 = chanx.NewClient[*Task](preCtx)
		pool.ctx3, pool.cnl3 = context.WithCancel(preCtx)
		go pool.taskCallBackMain()
	}
	return pool
}
func (obj *Client) taskCallBackMain() {
	defer obj.cnl3()
	defer obj.Close()
	defer obj.tasks2.Close()
	for {
		select {
		case task := <-obj.tasks2.Chan():
			select {
			case <-obj.ctx2.Done(): //接到关闭线程通知
				return
			case <-task.Done():
				if task.Error() != nil { //任务报错，线程报错
					obj.err = task.Error()
					return
				}
				if err := obj.taskDoneCallBack(task); err != nil { //任务回调报错，关闭线程
					obj.err = err
					return
				}
			}
		case <-obj.ctx2.Done(): //接到关闭线程通知
			return
		case <-obj.tasks2.Ctx().Done(): //chanx 关闭
			return
		}
	}
}
func (obj *Client) runMain() {
	var runVal any
	var err error
	threadId := obj.maxThreadId.Add(1) //获取线程id
	defer func() {
		if obj.clearThreadValue != nil && runVal != nil { //处理回调
			obj.clearThreadValue(obj.ctx, runVal)
		}
		select {
		case obj.threadTokens <- struct{}{}: //通知有一个协程空闲
		default:
		}
		select {
		case obj.dones <- struct{}{}: //通知协程结束
		default:
		}
	}()
	if obj.createThreadValue != nil { //线程开始回调
		runVal, err = obj.createThreadValue(obj.ctx, threadId)
		if err != nil {
			return
		}
	}
	for {
		if obj.runAfterTime == nil {
			obj.runAfterTime = time.NewTimer(time.Second * 30)
		} else {
			obj.runAfterTime.Reset(time.Second * 30)
		}
		select {
		case <-obj.ctx2.Done(): //通知线程关闭
			return
		case <-obj.ctx.Done(): //通知完成任务后关闭
			select {
			case <-obj.ctx2.Done(): //通知线程关闭
				return
			case task := <-obj.tasks: //接收任务
				obj.run(task, runVal, threadId) //运行任务
			default: //没有任务关闭线程
				return
			}
		case task := <-obj.tasks: //接收任务
			obj.run(task, runVal, threadId)
		case <-obj.runAfterTime.C: //等待线程超时
			return
		}
	}
}

var ErrPoolClosed = errors.New("pool closed")

func (obj *Client) verify(fun any, args []any) error {
	if fun == nil {
		return errors.New("not func")
	}
	typeOfFun := reflect.TypeOf(fun)
	index := 1
	if obj.createThreadValue != nil {
		index = 2
	}
	if typeOfFun.Kind() != reflect.Func {
		return errors.New("not func")
	}
	if typeOfFun.NumIn() != len(args)+index {
		return errors.New("args num error")
	}
	if typeOfFun.In(0).String() != "context.Context" {
		return errors.New("frist params not context.Context")
	}
	for i := index; i < len(args)+index; i++ {
		if args[i-index] == nil {
			if reflect.Zero(typeOfFun.In(i)).Interface() != args[i-index] {
				return errors.New("args type not equel")
			}
		} else if !reflect.TypeOf(args[i-index]).ConvertibleTo(typeOfFun.In(i)) {
			return errors.New("args type not equel")
		}
	}
	return nil
}

// 创建task
func (obj *Client) Write(task *Task) (*Task, error) {
	task.ctx, task.cnl = context.WithCancel(obj.ctx2) //设置任务ctx
	err := obj.verify(task.Func, task.Args)
	defer func() {
		if err != nil {
			task.err = err
			task.cnl()
		}
	}()
	if err != nil { //验证参数
		return task, err
	}
loop:
	for {
		select {
		case <-obj.ctx2.Done(): //接到线程关闭通知
			err = ErrPoolClosed
			break loop
		case <-obj.ctx.Done(): //接到线程关闭通知
			err = ErrPoolClosed
			break loop
		case obj.tasks <- task:
			if obj.tasks2 != nil {
				err = obj.tasks2.Add(task)
			}
			break loop
		case <-obj.threadTokens: //tasks 写不进去，线程池空闲，开启新的协程消费
			go obj.runMain()
		}
	}
	if oeerr := obj.Err(); oeerr != nil {
		err = oeerr
	}
	return task, err
}

type myInt int64

var ThreadId myInt = 0

func GetThreadId(ctx context.Context) int64 { //获取线程id，获取失败返回0
	if ctx == nil {
		return 0
	}
	if val := ctx.Value(ThreadId); val != nil {
		if v, ok := val.(int64); ok {
			return v
		}
	}
	return 0
}
func (obj *Client) run(task *Task, option any, threadId int64) {
	defer func() {
		if r := recover(); r != nil {
			task.err = fmt.Errorf("%v", r)
			if obj.debug {
				debug.PrintStack()
			}
		}
		task.cnl() //函数结束
	}()
	task.stat = 1
	//start
	index := 1
	if obj.createThreadValue != nil {
		if option == nil {
			task.err = errors.New("thread value is nil")
			return
		}
		if reflect.TypeOf(option).String() != reflect.TypeOf(task.Func).In(1).String() {
			task.err = fmt.Errorf("第二个参数类型不对: %T", option)
			return
		}
		index = 2
	}
	task.stat = 2
	//create ctx
	timeOut := task.Timeout
	if timeOut > 0 {
		task.ctx, task.cnl = context.WithTimeout(task.ctx, timeOut)
	}
	ctx := context.WithValue(task.ctx, ThreadId, threadId) //线程id 值写入ctx
	task.stat = 3
	//create params
	params := make([]reflect.Value, len(task.Args)+index)
	params[0] = reflect.ValueOf(ctx)
	if obj.createThreadValue != nil {
		params[1] = reflect.ValueOf(option)
	}
	for k, param := range task.Args {
		params[k+index] = reflect.ValueOf(param)
	}
	//run func
	task.stat = 4
	task.result = []any{}
	for _, rs := range reflect.ValueOf(task.Func).Call(params) { //执行主方法
		task.result = append(task.result, rs.Interface())
	}
	//end
	task.stat = 6
}

func (obj *Client) JoinClose() error { //等待所有任务完成，并关闭pool
	if obj.runAfterTime != nil {
		defer obj.runAfterTime.Stop()
	}
	obj.cnl()
	if obj.tasks2 != nil {
		obj.tasks2.JoinClose()
		<-obj.ctx3.Done()
	}
	if obj.ThreadSize() <= 0 {
		obj.cnl2()
		return obj.Err()
	}
	for {
		select {
		case <-obj.ctx2.Done(): //线程关闭推出
			return obj.Err()
		case <-obj.dones:
			if obj.ThreadSize() <= 0 {
				obj.cnl2()
				return obj.Err()
			}
		}
	}
}

func (obj *Client) Close() { //告诉所有协程，立即结束任务
	if obj.runAfterTime != nil {
		defer obj.runAfterTime.Stop()
	}
	obj.cnl()
	if obj.tasks2 != nil {
		obj.tasks2.Close()
	}
	obj.cnl2()
}
func (obj *Client) Err() error { //错误
	return obj.err
}
func (obj *Client) Done() <-chan struct{} { //所有任务执行完毕
	return obj.ctx2.Done()
}
func (obj *Client) ThreadSize() int { //创建的协程数量
	return obj.maxNum - len(obj.threadTokens)
}
func (obj *Client) Empty() bool { //任务是否为空
	if obj.ThreadSize() <= 0 && len(obj.tasks) == 0 {
		return true
	}
	return false
}
