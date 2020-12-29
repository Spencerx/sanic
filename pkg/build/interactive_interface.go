package build

import (
	"fmt"
	"github.com/layer-devops/sanic/pkg/util"
	"github.com/gdamore/tcell"
	"sort"
	"strings"
	"sync"
	"time"
)

type interactiveInterfaceJob struct {
	lastLogLines   *util.StringRingBuffer
	linesDisplayed int //used at rendering time
	status         string
	pushing        bool
	image          string
	service        string
}

type interactiveInterface struct {
	jobs            map[string]*interactiveInterfaceJob
	mutex           sync.Mutex
	screen          tcell.Screen
	screenStyle     tcell.Style
	cancelled       bool
	running         bool
	cancelListeners []func()
}

//NewInteractiveInterface creates and initializes a new tcell screen and event loop for use as an Interface
func NewInteractiveInterface() (Interface, error) {
	iface := &interactiveInterface{
		screenStyle: tcell.StyleDefault,
		jobs:        make(map[string]*interactiveInterfaceJob),
		running:     true,
	}

	tcell.SetEncodingFallback(tcell.EncodingFallbackFail)
	screen, err := tcell.NewScreen()
	if err != nil {
		return nil, err
	}
	if err = screen.Init(); err != nil {
		return nil, err
	}
	screen.Clear()

	go func() {
		for {
			ev := screen.PollEvent()
			if ev == nil {
				return
			}
			switch typedEvent := ev.(type) {
			case *tcell.EventResize:
				iface.redrawScreen()
				screen.Sync()
			case *tcell.EventKey:
				switch typedEvent.Key() {
				case tcell.KeyCtrlC, tcell.KeyEsc, tcell.KeyExit:
					for _, cancel := range iface.cancelListeners {
						cancel()
					}
					iface.cancelled = true
					return
				}
			}
		}
	}()

	go func() {
		for iface.running {
			iface.redrawScreen()
			time.Sleep(time.Millisecond * 150)
		}
	}()

	iface.screen = screen

	return iface, nil
}

func (iface *interactiveInterface) redrawScreen() {
	defer func() {
		r := recover()
		if r != nil {
			iface.Close()
			panic(r)
		}
	}()

	width, height := iface.screen.Size()

	iface.mutex.Lock()
	defer iface.mutex.Unlock()

	var succeededJobs []*interactiveInterfaceJob
	var failedJobs []*interactiveInterfaceJob
	var currJobs []*interactiveInterfaceJob

	for _, job := range iface.jobs {
		switch job.status {
		case "succeeded":
			succeededJobs = append(succeededJobs, job)
		case "failed":
			failedJobs = append(failedJobs, job)
		default:
			currJobs = append(currJobs, job)
		}
	}

	sortJobs := func(jobs []*interactiveInterfaceJob) {
		sort.Slice(jobs, func(i, j int) bool {
			return jobs[i].service < jobs[j].service
		})
	}
	sortJobs(succeededJobs)
	sortJobs(failedJobs)
	sortJobs(currJobs)

	displayAndTruncateString := func(y int, s string, style tcell.Style) {
		runes := []rune(s)
		for i := 0; i < width && i < len(runes); i++ {
			iface.screen.SetContent(i, y, runes[i], []rune{}, style)
		}
		for i := len(runes); i < width; i++ {
			iface.screen.SetContent(i, y, ' ', []rune{}, style)
		}
	}

	numFailedAndBuilding := len(failedJobs) + len(currJobs)
	if numFailedAndBuilding == 0 {
		return
	}

	currRenderLine := 0
	linesPerJob := (height - 1) / numFailedAndBuilding
	if linesPerJob < 2 {
		linesPerJob = 2
	}
	numRemainderLines := height - 1 - linesPerJob*numFailedAndBuilding

	failureStyle := iface.screenStyle.Foreground(tcell.NewRGBColor(190, 0, 0))
	for _, job := range failedJobs {
		if currRenderLine+1 >= height-2 {
			break
		}
		displayAndTruncateString(currRenderLine, "[failed] "+job.image, failureStyle)
		currRenderLine++
		logLinesToDisplay := linesPerJob - 1
		if numRemainderLines > 0 {
			logLinesToDisplay++
			numRemainderLines--
		}
		for _, logLine := range job.lastLogLines.Peek(logLinesToDisplay) {
			displayAndTruncateString(currRenderLine, logLine, iface.screenStyle)
			currRenderLine++
		}
	}

	currStyle := iface.screenStyle.Foreground(tcell.NewRGBColor(190, 190, 0))
	for _, job := range currJobs {
		if currRenderLine+1 >= height-2 {
			break
		}
		status := "[building]"
		if job.pushing {
			status = "[building/pushing]"
		}
		displayAndTruncateString(currRenderLine, status+" "+job.image, currStyle)
		currRenderLine++
		logLinesToDisplay := linesPerJob - 1
		if numRemainderLines > 0 {
			logLinesToDisplay++
			numRemainderLines--
		}
		for _, logLine := range job.lastLogLines.Peek(logLinesToDisplay) {
			displayAndTruncateString(currRenderLine, logLine, iface.screenStyle)
			currRenderLine++
		}
	}

	numJobs := len(currJobs) + len(failedJobs) + len(succeededJobs)
	statusStyle := iface.screenStyle.Foreground(tcell.NewRGBColor(190, 190, 190))
	displayAndTruncateString(
		height-1,
		fmt.Sprintf(
			"%d/%d failed, %d/%d completed, %d/%d building",
			len(failedJobs), numJobs,
			len(succeededJobs), numJobs,
			len(currJobs), numJobs,
		),
		statusStyle,
	)

	iface.screen.Show()
}

func (iface *interactiveInterface) Close() {
	iface.mutex.Lock()
	defer iface.mutex.Unlock()

	iface.running = false
	iface.screen.Fini()
	var serviceLogDirs []string
	var serviceImages []string
	var failedJobs []string
	for jobName, job := range iface.jobs {
		serviceLogDirs = append(serviceLogDirs, fmt.Sprintf("logs/%s.log", jobName)) //TODO messy
		serviceImages = append(serviceImages, job.image)
		if job.status != "succeeded" {
			failedJobs = append(failedJobs, jobName)
		}
	}

	if !iface.cancelled {
		if len(failedJobs) > 0 {
			fmt.Printf("Failed to build the following jobs: %s\nSee the logs folder for details.\n", strings.Join(failedJobs, ", "))
		} else {
			fmt.Printf("Successfully built: %s\n", strings.Join(serviceImages, " "))
		}
	}

}

func (iface *interactiveInterface) StartJob(service string, image string) {
	iface.mutex.Lock()
	defer iface.mutex.Unlock()

	iface.jobs[service] = &interactiveInterfaceJob{
		service:      service,
		image:        image,
		lastLogLines: util.CreateStringRingBuffer(20),
	}
}

func (iface *interactiveInterface) FailJob(service string, err error) {
	iface.ProcessLog(service, "Error! " + err.Error())

	iface.mutex.Lock()
	defer iface.mutex.Unlock()

	if job, ok := iface.jobs[service]; ok {
		job.status = "failed"
	}
}

func (iface *interactiveInterface) SucceedJob(service string) {
	iface.mutex.Lock()
	defer iface.mutex.Unlock()

	if job, ok := iface.jobs[service]; ok {
		job.status = "succeeded"
	}
}

func (iface *interactiveInterface) SetPushing(service string) {
	iface.mutex.Lock()
	defer iface.mutex.Unlock()

	if job, ok := iface.jobs[service]; ok {
		job.pushing = true
	}
}

func (iface *interactiveInterface) ProcessLog(service, logLine string) {
	iface.mutex.Lock()
	defer iface.mutex.Unlock()

	job, ok := iface.jobs[service]
	if !ok {
		panic("Could not find service: " + service)
	}
	logLine = strings.TrimSpace(logLine)
	if logLine != "" {
		job.lastLogLines.Push(logLine)
		//notice: server time might drift, so we use local time
	}
}

func (iface *interactiveInterface) AddCancelListener(cancelFunc func()) {
	iface.cancelListeners = append(iface.cancelListeners, cancelFunc)
}
