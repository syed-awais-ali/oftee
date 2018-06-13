// OFTEE command to start a OpenFlow tee proxy. This command parses the
// environment for configuration information and then starts a processing
// loop for OpenFlow messages.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/ciena/oftee/connections"
	"github.com/ciena/oftee/criteria"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/kelseyhightower/envconfig"
	of "github.com/netrack/openflow"
	"github.com/netrack/openflow/ofp"
	log "github.com/sirupsen/logrus"
)

const (
	// Buffer size when reading
	BUFFER_SIZE = 2048

	// Supported and future supported URL schemes
	SCHEME_TCP   = "tcp"
	SCHEME_HTTP  = "http"
	SCHEME_KAFKA = "kafka"

	// Supported end point configuration terms
	TERM_ACTION  = "action"
	TERM_DL_TYPE = "dl_type"
)

// Maintains the application configuration and runtime state
type App struct {
	ShowHelp         bool     `envconfig:"HELP" default:"false" desc:"show this message"`
	ListenOn         string   `envconfig:"LISTEN_ON" default:":8000" required:"true" desc:"connection on which to listen for an open flow device"`
	ProxyTo          string   `envconfig:"PROXY_TO" default:":8001" required:"true" desc:"connection on which to attach to an SDN controller"`
	TeeTo            []string `envconfig:"TEE_TO" default:":8002" desc:"list of connections on which tee packet in messages"`
	LogLevel         string   `envconfig:"LOG_LEVEL" default:"debug" desc:"logging level"`
	ShareConnections bool     `envconfig:"SHARE_CONNECTIONS" default:"true" desc:"use shared connections to outbound end points"`

	listener  net.Listener
	endpoints connections.Endpoints
}

// Why or why does Go not have a simply int minimum function, ok, i get it,
// proverb A little copying is better than a little dependency, but this could
// be part of a standard lib
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Handle a single connection from a device
func (app *App) handle(conn net.Conn, endpoints connections.Endpoints) {
	var err error
	var buffer *bytes.Buffer = new(bytes.Buffer)
	var match criteria.Criteria
	var header of.Header
	var hCount, piCount int64
	var left uint16
	var count int
	var packetIn ofp.PacketIn

	// Create connection to SDN controller
	proxy := new(connections.TcpConnection)
	proxy.Connection, err = net.Dial("tcp", app.ProxyTo)
	proxy.Criteria = criteria.Criteria{}

	// Anything from the controller, just send to the device
	go io.Copy(conn, proxy.Connection)

	reader := bufio.NewReaderSize(conn, BUFFER_SIZE)
	buf := make([]byte, BUFFER_SIZE)
	for {
		// Read open flow header, if this does not work then we have
		// a serious error, so fail fast and move on
		hCount, err = header.ReadFrom(reader)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err.Error(),
			}).Debug("Failed to read OpenFlow message header")
			break
		}

		// If we have a packet in message then this will be tee-ed
		// to those end points that match, else we just proxy to
		// the controller.
		if header.Type == of.TypePacketIn {
			log.WithFields(log.Fields{
				"of_version":     header.Version,
				"of_message":     header.Type.String(),
				"of_transaction": header.Transaction,
				"length":         header.Length,
			}).Debug("SENDING: all end-points")

			// Read the packet in message header
			piCount, err = packetIn.ReadFrom(reader)
			if err != nil {
				log.WithFields(log.Fields{
					"error": err.Error(),
				}).Debug("Failed to read OpenFlow Packet In message header")
				break
			}

			// Reset the buffer to read the packet in message and
			// write the headers to the buffer
			buffer.Reset()
			header.WriteTo(buffer)
			packetIn.WriteTo(buffer)

			// Read and buffer the rest of the packet
			left = header.Length - uint16(hCount) - uint16(piCount)
			for left > 0 {
				count, err = reader.Read(buf[:min(int(left), BUFFER_SIZE)])
				if err != nil {
					log.Debugf("ERROR %s", err)
					break
				}
				buffer.Write(buf[:count])
				left -= uint16(count)
			}

			// Load and decode the packet being packeted in so we
			// can compare match criteria
			pkt := gopacket.NewPacket(buffer.Bytes()[hCount+piCount:],
				layers.LayerTypeEthernet,
				gopacket.DecodeOptions{Lazy: true, NoCopy: true})
			eth := pkt.Layer(layers.LayerTypeEthernet)
			match = criteria.Criteria{
				Set:    criteria.BIT_DL_TYPE,
				DlType: uint16(eth.(*layers.Ethernet).EthernetType),
			}

			// packet out to the SDN controller and packet out
			// to those end points that match the criteria
			proxy.Write(buffer.Bytes())
			// TODO loop until all bytes are written
			// TODO if error is returned and connection is broken, reconnect

			endpoints.ConditionalWrite(buffer.Bytes(), match)
			// TODO loop until all bytes are written
			// TODO if error is returned and connection is broken, reconnect
		} else {
			// All messages that are not packet in messages are
			// only proxied to the SDN controller. No buffering,
			// just grab bits, push bits.
			log.WithFields(log.Fields{
				"of_version":     header.Version,
				"of_message":     header.Type.String(),
				"of_transaction": header.Transaction,
				"length":         header.Length,
			}).Debug("SENDING: SDN controller")
			header.WriteTo(proxy)
			// TODO if error is returned and connection is broken, reconnect

			left = header.Length - uint16(hCount)
			for left > 0 {
				count, err = reader.Read(buf[:min(int(left), BUFFER_SIZE)])
				if err != nil {
					log.Debugf("ERROR %s", err)
					break
				}
				proxy.Write(buf[:count])
				// TODO loop until all bytes are written
				// TODO if error is returned and connection is broken, reconnect

				left -= uint16(count)
			}
		}
	}
}

func (app *App) EstablishEndpointConnections() (connections.Endpoints, error) {
	var u *url.URL
	var c connections.Connection
	var hc *connections.HttpConnection
	var tcp *connections.TcpConnection
	var match criteria.Criteria
	var addr string
	var parts, terms []string
	var err error

	endpoints := make([]connections.Connection, len(app.TeeTo))

	for i, spec := range app.TeeTo {
		if len(spec) != 0 {

			// The connection address is of the form
			//    [match],action=url
			// Where [match] is a list of match terms, currently
			// only dl_type is supported.
			parts = strings.Split(spec, ";")
			match = criteria.Criteria{}
			if len(parts) == 1 {
				addr = spec
			} else {
				addr = ""
				for _, part := range parts {
					terms = strings.Split(part, "=")
					switch strings.ToLower(terms[0]) {
					case TERM_ACTION:
						addr = terms[1]
					case TERM_DL_TYPE:
						ethType, err := strconv.ParseUint(terms[1], 0, 16)
						match.Set |= criteria.BIT_DL_TYPE
						match.DlType = uint16(ethType)
						if err != nil {
							log.WithFields(log.Fields{
								"term":  terms[0],
								"value": terms[1],
							}).Error("Unable to convert term to uint16")
							return nil, err
						}
						log.WithFields(log.Fields{
							"term":  terms[0],
							"value": terms[1],
						}).Debug("Found condition")
					default:
						log.WithFields(log.Fields{
							"term":  terms[0],
							"value": terms[1],
						}).Error("Unknown end point term")
						return nil, fmt.Errorf("Unknown end point term '%s'", terms[0])
					}
				}
			}

			// Read schema from connection string
			u, err = url.Parse(addr)
			if err != nil {
				log.WithFields(log.Fields{
					"connect": addr,
					"error":   err,
				}).Error("Unable to parse connection string")
				return nil, err
			}
			switch strings.ToLower(u.Scheme) {
			default:
				u.Host = addr
				fallthrough
			case SCHEME_TCP:
				tcp = new(connections.TcpConnection)
				tcp.Connection, err = net.Dial("tcp", u.Host)
				tcp.Criteria = match
				c = *tcp
			case SCHEME_HTTP:
				hc = new(connections.HttpConnection)
				hc.Connection = *u
				hc.Criteria = match
				c = *hc
				err = nil
			}
			if err != nil {
				log.WithFields(log.Fields{
					"connection": addr,
					"error":      err,
				}).Error("Unable to connect to outbound end point")
				return nil, err
			} else {
				log.WithFields(log.Fields{
					"connection": addr,
					"c":          c,
					"tcp":        tcp,
					"hc":         hc,
					"host":       u.Host,
				}).Infof("Created outbound end point connection")
				endpoints[i] = c
			}
		}
	}
	return endpoints, nil
}

// Listen for connections from open flow devices and process their
// messages
func (app *App) ListenAndServe() (err error) {
	// Bind to connection for accepting connections
	app.listener, err = net.Listen("tcp", app.ListenOn)
	if err != nil {
		log.Fatalf("Unable to establish the ability to listen on connection '%s' : %s", app.ListenOn, err)
	}

	// Loop forever waiting for a connection and processing it
	endpoints := app.endpoints
	for {
		conn, err := app.listener.Accept()
		if err != nil {
			// Not fatal if a connection fails, forget it and move on
			log.WithFields(log.Fields{
				"error": err,
			}).Error("Error while accepting connection")
			continue
		}
		log.Debugf("Received connection: %s", conn.RemoteAddr().String())
		endpoints = app.endpoints
		if !app.ShareConnections {
			endpoints, err = app.EstablishEndpointConnections()
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
				}).Error("Unable to establish non-shared outbound endpoint connections")
				continue
			}
		}
		go app.handle(conn, endpoints)
	}
}

func main() {
	var app App

	// This application is not configured by command line options, so
	// if we have an unknown options or they used -h/--help to ask for
	// usage, give it to them
	var flags flag.FlagSet
	err := flags.Parse(os.Args[1:])
	if err != nil {
		envconfig.Usage("", &(app))
		return
	}

	// Load the application configuration from the environment and initialize
	// the logging system
	err = envconfig.Process("", &app)
	if err != nil {
		log.Fatalf("Unable to parse configuration : %s\n", err)
	}

	// Set the logging level, if it can't be parsed then default to warning
	logLevel, err := log.ParseLevel(app.LogLevel)
	if err != nil {
		log.Warnf("Unable to parse log level specififed '%s', defaulting to 'warning' : %s", app.LogLevel, err)
		logLevel = log.WarnLevel
	}
	log.SetLevel(logLevel)

	// If the help message is requested, then display and return
	if app.ShowHelp {
		envconfig.Usage("", &app)
		return
	}

	// Connect to shared outbound end point connections, if requested
	if app.ShareConnections {
		if app.endpoints, err = app.EstablishEndpointConnections(); err != nil {
			log.Fatalf("Unable to establish connections to outbound end points, terminating : %s",
				err.Error())
		}
	}

	// Listen and serve requests
	log.Fatal(app.ListenAndServe())
}
