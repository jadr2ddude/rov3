package main

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jadr2ddude/ds4"
	"github.com/urfave/cli"
)

func bound(x float64, min float64, max float64) float64 {
	if x < min {
		return min
	}
	if x > max {
		return max
	}
	return x
}

func mapVal(x float64, inmin float64, inmax float64, outmin float64, outmax float64) float64 {
	return bound((x-inmin)*(outmax-outmin)/(inmax-inmin)+outmin, outmin, outmax)
}

const maxtilt = math.Pi / 3
const maxvert = 1.0

func main() {
	app := cli.NewApp()
	app.Name = "top side"
	app.Usage = "run top side of robot"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "bottomurl",
			Usage: "bottom side URL",
		},
		cli.StringFlag{
			Name:  "static",
			Usage: "path to static files",
		},
	}
	app.Action = func(c *cli.Context) error {
		log.Printf("Static: %q\n", c.GlobalString("static"))
		//set default router to static files
		http.Handle("/", http.FileServer(http.Dir(c.GlobalString("static"))))
		//set up reverse proxies to bottom side
		burl, err := url.Parse(c.GlobalString("bottomurl"))
		if err != nil {
			panic(err)
		}
		rp := httputil.NewSingleHostReverseProxy(burl)
		http.HandleFunc("/bs/", func(w http.ResponseWriter, r *http.Request) {
			pth, err := filepath.Rel("/bs/", r.URL.Path)
			if err != nil {
				http.Error(w, fmt.Sprintf("path processing error: %q", err.Error()), http.StatusBadRequest)
				return
			}
			r.URL.Path = pth
			rp.ServeHTTP(w, r)
		})
		//handle dualshock
		go func() { //search for devices in the background
			//initial state
			var bs BotState
			//HUD state
			var hs struct {
				ClawLocked bool `json:"clawlocked"`
				ClawOpen   int  `json:"clawopen"`
			}
			//connect to control system
			ws, _, err := websocket.DefaultDialer.Dial(
				burl.String(),
				http.Header{
					"Origin": []string{
						burl.Hostname(),
					},
				},
			)
			if err != nil {
				log.Fatalf("Failed to connect to control system: %q\n", err.Error())
			}
			defer ws.Close()
			cslck := new(sync.Mutex)
			csch := make(chan struct{})
			c := &ds4.Controller{
				NoClose: true, //re-use
				Joysticks: struct {
					Left, Right ds4.Joystick
				}{
					Left: ds4.Joystick{ //movement
						X: make(chan float64),
						Y: make(chan float64),
						//not using button
					},
					Right: ds4.Joystick{ //claw
						X:   make(chan float64),
						Y:   make(chan float64),
						Btn: make(ds4.ButtonChannel), //hold position
					},
				},
				//D-Pad not used
				Touchpad: ds4.TouchPad{ //used for tilting
					Touches: make(chan []ds4.TouchPadReading),
				},
				//Top right buttons not used
				//Playstation button not used
				//Share/Option buttons not used
				X:      make(ds4.ButtonChannel), //open claw
				Circle: make(ds4.ButtonChannel), //close claw
				PSButton: ds4.ButtonPRChannel{
					Push: make(chan struct{}), //light toggle
					//release unused
				},
				Triangle: make(ds4.ButtonChannel), //obs noise
				L2:       make(chan float64),      //moving up
				R2:       make(chan float64),      //moving down
			}
			_, _, _ = bs, hs, c
			go func() { //search for dualshock devices
				for {
					time.Sleep(time.Second)
					evd, err := ds4.SearchEvdev()
					if err != nil {
						log.Printf("Evdev search failed: %q\n", err.Error())
					}
					if len(evd) == 0 {
						continue
					}
					for _, v := range evd {
						cslck.Lock()
						csch <- struct{}{}
						cslck.Lock()
						c.Dev = v
						cslck.Unlock()
						csch <- struct{}{}
						c.Run()
					}
				}
			}()
			tick := time.NewTicker(time.Second / 25)
			tick2 := time.NewTicker(time.Second / 100)
			hs.ClawLocked = true //lock claw by default
			for {
				select {
				case <-csch: //handle device swap
					cslck.Unlock() //let the swapper go
					<-csch         //wait for swap to finish
				case <-tick.C: //send BotState update
					err := ws.WriteJSON(bs)
					if err != nil {
						log.Fatalf("Failed to send botstate: %q\n", err.Error())
					}
				case <-tick2.C: //update claw servo angle
					co := int(bs.ClawOpen) + hs.ClawOpen
					if co > 180 {
						co = 180
					} else if co < 0 {
						co = 0
					}
					bs.ClawOpen = uint8(co)
				case v := <-c.Joysticks.Left.X:
					bs.Turn = v
				case v := <-c.Joysticks.Left.Y:
					bs.Forward = v
				case v := <-c.Joysticks.Right.X:
					if hs.ClawLocked {
						continue
					}
					bs.ClawHorizontal = v
				case v := <-c.Joysticks.Right.Y:
					if hs.ClawLocked {
						continue
					}
					bs.ClawVert = uint8(mapVal(v, -1, 1, 0, 180))
				case lck := <-c.Joysticks.Right.Btn.(ds4.ButtonChannel):
					hs.ClawLocked = !lck
				case tr := <-c.Touchpad.Touches:
					if len(tr) != 1 {
						bs.TiltX = 0
						bs.TiltY = 0
					} else {
						bs.TiltX = tr[0].X * maxtilt
						bs.TiltY = tr[0].Y * maxtilt
					}
				case v := <-c.L2:
					bs.Vertical = v * maxvert
				case v := <-c.R2:
					bs.Vertical = -v * maxvert
				case v := <-c.X.(ds4.ButtonChannel):
					if v {
						hs.ClawOpen = 1
					} else {
						hs.ClawOpen = 0
					}
				case v := <-c.Circle.(ds4.ButtonChannel):
					if v {
						hs.ClawOpen = -1
					} else {
						hs.ClawOpen = 0
					}
				case <-c.PSButton.(ds4.ButtonPRChannel).Push:
					bs.Light = !bs.Light
					/*case v := <-c.PSButton.(ds4.ButtonChannel):
					bs.OBSSound = v*/
				}
			}
		}()
		//http.Handle("/", r)
		log.Fatalf("Failed to serve: %q\n", http.ListenAndServe(":8001", nil).Error())
		return nil
	}
	app.Run(os.Args)
}

//BotState is the state of the robot
type BotState struct {
	Forward        float64 //between -1 and 1
	Turn           float64 //between -1 and 1
	Vertical       float64 //in m/s^2
	TiltX, TiltY   float64 //in radians
	ClawOpen       uint8   //is the claw supposed to be open
	ClawVert       uint8   //claw vertical tilt
	ClawHorizontal float64 //claw horizontal tilt
	UpdateCount    uint64  //number of times the motors have beeen updated
	Light          bool
	OBSSound       bool
}
