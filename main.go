package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishen/go-chromecast/application"
	castdns "github.com/vishen/go-chromecast/dns"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/physic"
	"periph.io/x/host/v3"
)

const (
	minPWM  = 10
	maxPWM  = 50
	gpioReg = "GPIO23"
	mp3URL  = "https://nightmare-doorbell.s3.us-east-2.amazonaws.com/nightmarecat.mp3"
)

type Doorbell struct {
	sync.RWMutex
	FlickerPins []gpio.PinIO
	Devices     map[string]Device
}

type Device struct {
	Addr string
	Port int

	Name string
	Host string

	UUID       string
	Device     string
	Status     string
	DeviceName string
	InfoFields map[string]string
}

func main() {
	// Load all the drivers:
	if _, err := host.Init(); err != nil {
		log.Fatal(err)
	}

	var doorbell Doorbell
	doorbell.FlickerPins = make([]gpio.PinIO, 2)
	doorbell.Devices = make(map[string]Device, 0)

	pwm1 := gpioreg.ByName("PWM1_OUT")
	if pwm1 == nil {
		log.Fatal("Failed to find PWM1_OUT")
	}
	doorbell.FlickerPins[0] = pwm1

	pwm2 := gpioreg.ByName("PWM0_OUT")
	if pwm2 == nil {
		log.Fatal("Failed to find PWM0.OUT")
	}
	doorbell.FlickerPins[1] = pwm2

	ctx, cancel := context.WithCancel(context.Background())
	doorbell.flicker(ctx)

	doorbell.waitForButton(ctx)

	doorbell.setGoogleHomeAddrs(ctx)

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	select {
	case <-sigc:
		fmt.Println("received stop signal")
		cancel()
		if err := pwm1.Halt(); err != nil {
			log.Fatal(err)
		}
		if err := pwm2.Halt(); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}
}

//Currently don't have a clean way to shut this down.  Need to think about it
func (doorbell *Doorbell) flicker(ctx context.Context) {
	fmt.Println("starting flickering lights")
	for idx, pin := range doorbell.FlickerPins {
		go func(ctx context.Context, p gpio.PinIO, pinid int) {
			for {
				rand.Seed(time.Now().UnixNano())
				randpwm := rand.Intn((maxPWM - minPWM + 1) + minPWM)
				randInterval := rand.Intn(10)
				for i := minPWM; i < randpwm; i++ {
					freq := physic.Frequency(i)
					if err := p.PWM(gpio.DutyHalf, freq*physic.Hertz); err != nil {
						log.Fatal(err)
					}
					time.Sleep(200 * time.Millisecond)
				}
				for i := randpwm; i > minPWM; i-- {
					freq := physic.Frequency(i)
					if err := p.PWM(gpio.DutyHalf, freq*physic.Hertz); err != nil {
						log.Fatal(err)
					}
					time.Sleep(200 * time.Millisecond)
				}

				if err := p.Halt(); err != nil {
					log.Fatal(err)
				}
				time.Sleep(time.Duration(randInterval) * time.Second)
			}
		}(ctx, pin, idx)
	}
}

func (doorbell *Doorbell) pinListener() chan gpio.Level {
	readings := make(chan gpio.Level, 10)
	go func() {
		fmt.Println("setting up button pin")
		p := gpioreg.ByName(gpioReg)
		if p == nil {
			log.Fatal("Failed to find ", gpioReg)
		}

		if err := p.In(gpio.PullUp, gpio.BothEdges); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("first reading: %s\n", p.Read())
		/*pin, err := gpioutil.Debounce(doorbell.SensorPin, 0, 10*time.Second, gpio.BothEdges)
		if err != nil {
			fmt.Errorf("Error: %w", err)
		}
		*/
		for {
			fmt.Println("waiting for button press")
			p.WaitForEdge(-1)
			read := p.Read()
			fmt.Printf("-> %s\n", read)
			readings <- read
		}
	}()

	return readings
}

func (doorbell *Doorbell) waitForButton(ctx context.Context) {
	fmt.Println("starting pin listener")
	go func(ctx context.Context) {
		readings := doorbell.pinListener()
		lastReading := time.Now()
		for {
			select {
			case <-ctx.Done():
				fmt.Println("stopping sensor input")
				/*if err := p.Halt(); err != nil {
					log.Fatal(err)
				}*/
				return
			case reading := <-readings:
				currentReading := time.Now()
				if currentReading.Sub(lastReading).Seconds() > 30 {
					fmt.Printf("select -> %s\n", reading)
					lastReading = time.Now()
					doorbell.Lock()

					for _, device := range doorbell.Devices {
						//Select certain speakers between 8PM and 8AM
						if currentReading.Hour() < 7 || currentReading.Hour() > 19 {
							fmt.Println("playing on subset of speakers")
							if strings.Contains(device.DeviceName, "Kitchen") || strings.Contains(device.DeviceName, "Bedroom") {
								if err := doorbell.connectDevice(device); err != nil {
									fmt.Println(err.Error())
								}
							}
						} else {
							if !strings.Contains(device.Name, "Group") {
								//yeet
								go func() {
									if err := doorbell.connectDevice(device); err != nil {
										fmt.Println(err.Error())
									}
								}()
							}
						}
					}
					doorbell.Unlock()
				}
			}
		}
	}(ctx)
	//Keeping this here for now, might revisit later for a notification system to alert phones too
	/*dataMap := make(map[string]map[string]bool, 1)
	var buf bytes.Buffer
	for {
		pin.WaitForEdge(-1)
		fmt.Printf("-> %s\n", p.Read())
		msg := make(map[string]bool, 1)
		msg["doorbell"] = true
		dataMap["data"] = msg
		data, err := json.Marshal(dataMap)
		if err != nil {
			fmt.Errorf("Error: %w", err)
			continue
		}
		buf.Write(data)
		resp, err := http.Post("https://pub-hub.home.rsmachiner.com/home/doorbell/events", "application/json", &buf)
		if err != nil {
			fmt.Errorf("Error %w", err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			fmt.Println("Unable to publish button press: ", resp.StatusCode)
			continue
		}
		fmt.Println("Successfully sent doorbell status")
	}*/
}

//setGoogleHomeAddrs scans the network every hour looking for devices
func (doorbell *Doorbell) setGoogleHomeAddrs(ctx context.Context) {
	go func(ctx context.Context) {

		iface, err := getNetworkInterface()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("found iface: %s\n", iface.Name)
		ticker := time.NewTicker(1 * time.Hour)
		for ; true; <-ticker.C { //Trick to get the ticker to fire once immediately and then every hour after
			fmt.Println("finding google home devices")
			castEntryChan, err := castdns.DiscoverCastDNSEntries(ctx, iface)
			if err != nil {
				fmt.Printf("unable to discover chromecast devices: %v\n", err)
			}
			for d := range castEntryChan {
				fmt.Printf("device=%q device_name=%q address=\"%s:%d\" uuid=%q\n", d.Device, d.DeviceName, d.AddrV4, d.Port, d.UUID)

				device := Device{
					Addr:       d.AddrV4.String(),
					Port:       d.Port,
					Name:       d.Name,
					Host:       d.Host,
					UUID:       d.UUID,
					Device:     d.Device,
					Status:     d.Status,
					DeviceName: d.DeviceName,
					InfoFields: d.InfoFields,
				}
				doorbell.Lock()
				doorbell.Devices[d.UUID] = device
				doorbell.Unlock()
			}
		}
	}(ctx)
}

//creating a new application every run, maybe inefficient?
func (doorbell *Doorbell) connectDevice(d Device) error {
	fmt.Println("connecting to device: ", d.DeviceName)
	appOptions := []application.ApplicationOption{
		application.WithDebug(true),
		application.WithCacheDisabled(true),
	}

	app := application.NewApplication(appOptions...)
	if err := app.Start(d.Addr, d.Port); err != nil {
		return fmt.Errorf("unable to start application: %v", err)
	}

	if err := app.Load(mp3URL, "", true, true, true); err != nil {
		fmt.Printf("unable to load media for device: %v\n", err)
	}

	if err := app.Close(false); err != nil {
		return fmt.Errorf("unable to close application %q: %v", d.UUID, err)
	}
	return nil
}

func getNetworkInterface() (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for _, iface := range interfaces {
		if strings.Contains(iface.Flags.String(), "up") && iface.Name != "lo" {
			return &iface, nil
		}
	}
	return nil, fmt.Errorf("no interfaces found")
}
