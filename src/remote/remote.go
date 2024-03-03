
package remote

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"reflect"
	"time"
)

// LeakySocket
//
// LeakySocket is a wrapper for a net.Conn connection that emulates
// transmission delays and random packet loss. it has its own send
// and receive functions that together mimic an unreliable connection
// that can be customized to stress-test remote service interactions.
type LeakySocket struct {
	s         net.Conn
	isLossy   bool
	lossRate  float32
	msTimeout int
	usTimeout int
	isDelayed bool
	msDelay   int
	usDelay   int
}

// builder for a LeakySocket given a normal socket and indicators
// of whether the connection should experience loss and delay.
// uses default loss and delay values that can be changed using setters.
func NewLeakySocket(conn net.Conn, lossy bool, delayed bool) *LeakySocket {
	ls := &LeakySocket{}
	ls.s = conn
	ls.isLossy = lossy
	ls.isDelayed = delayed
	ls.msDelay = 2
	ls.usDelay = 0
	ls.msTimeout = 500
	ls.usTimeout = 0
	ls.lossRate = 0.05

	return ls
}

// send a byte-string over the socket mimicking unreliability.
// delay is emulated using time.Sleep, packet loss is emulated using RNG
// coupled with time.Sleep to emulate a timeout
func (ls *LeakySocket) SendObject(obj []byte) (bool, error) {
	if obj == nil {
		return true, nil
	}

	if ls.s != nil {
		rand.Seed(time.Now().UnixNano())
		if ls.isLossy && rand.Float32() < ls.lossRate {
			time.Sleep(time.Duration(ls.msTimeout)*time.Millisecond + time.Duration(ls.usTimeout)*time.Microsecond)
			return false, nil
		} else {
			if ls.isDelayed {
				time.Sleep(time.Duration(ls.msDelay)*time.Millisecond + time.Duration(ls.usDelay)*time.Microsecond)
			}
			_, err := ls.s.Write(obj)
			if err != nil {
				return false, errors.New("SendObject Write error: " + err.Error())
			}
			return true, nil
		}
	}
	return false, errors.New("SendObject failed, nil socket")
}

// receive a byte-string over the socket connection.
// no significant change to normal socket receive.
func (ls *LeakySocket) RecvObject() ([]byte, error) {
	if ls.s != nil {
		buf := make([]byte, 4096)
		n := 0
		var err error
		for n <= 0 {
			n, err = ls.s.Read(buf)
			if n > 0 {
				return buf[:n], nil
			}
			if err != nil {
				if err != io.EOF {
					return nil, errors.New("RecvObject Read error: " + err.Error())
				}
			}
		}
	}
	return nil, errors.New("RecvObject failed, nil socket")
}

// enable/disable emulated transmission delay and/or change the delay parameter
func (ls *LeakySocket) SetDelay(delayed bool, ms int, us int) {
	ls.isDelayed = delayed
	ls.msDelay = ms
	ls.usDelay = us
}

// change the emulated timeout period used with packet loss
func (ls *LeakySocket) SetTimeout(ms int, us int) {
	ls.msTimeout = ms
	ls.usTimeout = us
}

// enable/disable emulated packet loss and/or change the loss rate
func (ls *LeakySocket) SetLossRate(lossy bool, rate float32) {
	ls.isLossy = lossy
	ls.lossRate = rate
}

// close the socket (can also be done on original net.Conn passed to builder)
func (ls *LeakySocket) Close() error {
	return ls.s.Close()
}

// RemoteObjectError
//
// RemoteObjectError is a custom error type used for this library to identify remote methods.
// it is used by both caller and callee endpoints.
type RemoteObjectError struct {
	Err string
}

// getter for the error message included inside the custom error type
func (e *RemoteObjectError) Error() string { return e.Err }

// RequestMsg (this is only a suggestion, can be changed)
//
// RequestMsg represents the request message sent from caller to callee.
// it is used by both endpoints, and uses the reflect package to carry
// arbitrary argument types across the network.
type RequestMsg struct {
	Method string
	// Args   []reflect.Value
	Args   []interface{}
}

// ReplyMsg (this is only a suggestion, can be changed)
//
// ReplyMsg represents the reply message sent from callee back to caller
// in response to a RequestMsg. it similarly uses reflection to carry
// arbitrary return types along with a success indicator to tell the caller
// whether the call was correctly handled by the callee. also includes
// a RemoteObjectError to specify details of any encountered failure.
type ReplyMsg struct {
	// 200 for success, 404 for method not found
	// 405 for method argument number not correct, 500 for method argument type not correct
	Success int
	// Reply   []reflect.Value
	Reply   []interface{}
	Err     RemoteObjectError
}

// Service -- server side stub/skeleton
//
// A Service encapsulates a multithreaded TCP server that manages a single
// remote object on a single TCP port, which is a simplification to ease management
// of remote objects and interaction with callers.  Each Service is built
// around a single struct of function declarations. All remote calls are
// handled synchronously, meaning the lifetime of a connection is that of a
// sinngle method call.  A Service can encounter a number of different issues,
// and most of them will result in sending a failure response to the caller,
// including a RemoteObjectError with suitable details.
type Service struct {
	InterfaceType reflect.Type		//       - reflect.Type of the Service's interface (struct of Fields)
	InterfaceValue reflect.Value	//       - reflect.Value of the Service's interface
	RemoteObject reflect.Value		//       - reflect.Value of the Service's remote object instance
	//       - status and configuration parameters, as needed
	LeakySocket *LeakySocket
	port int
	lossy bool
	delayed bool
	listener net.Listener
	running bool
}

// build a new Service instance around a given struct of supported functions,
// a local instance of a corresponding object that supports these functions,
// and arguments to support creation and use of LeakySocket-wrapped connections.
// performs the following:
// -- returns a local error if function struct or object is nil
// -- returns a local error if any function in the struct is not a remote function
// -- if neither error, creates and populates a Service and returns a pointer
func NewService(ifc interface{}, sobj interface{}, port int, lossy bool, delayed bool) (*Service, error) {
	// check if ifc equals nil
	if ifc == nil {
		return nil, errors.New("ifc is nil")
	}
	// check if sobj equals nil
	if sobj == nil {
		return nil, errors.New("sobj is nil")
	}

	// make sure that ifc is an interface to a struct
	if reflect.TypeOf(ifc).Kind() != reflect.Ptr || reflect.TypeOf(ifc).Elem().Kind() != reflect.Struct {
		return nil, errors.New("ifc is not a pointer to a struct")
	}
	// get ifc type
	ifcType := reflect.TypeOf(ifc).Elem()
	// check if ifc's Method supports RemoteObjectError
	for i := 0; i < ifcType.NumField(); i++ {
		method := ifcType.Field(i)
		if method.Type.Out(method.Type.NumOut()-1) != reflect.TypeOf(RemoteObjectError{}) {
			log.Println(method.Type.Out(method.Type.NumOut()-1), reflect.TypeOf(RemoteObjectError{}))
			return nil, errors.New("method does not follow remote call convention")
		}
	}

	// get sobj value
	sobjValue := reflect.ValueOf(sobj)

	service := &Service{
		InterfaceType:  ifcType,
		InterfaceValue: reflect.Zero(ifcType),
		RemoteObject:   sobjValue,
		LeakySocket: nil,
		port: port,
		lossy: lossy,
		delayed: delayed,
		listener: nil,
		running: false,
	}

	return service, nil
}

// start the Service's tcp listening connection, update the Service
// status, and start receiving caller connections
func (serv *Service) Start() error {
    if serv.IsRunning() {
        log.Println("Warning: Service is already running")
        return nil
    }

    // start TCP listener connection
    addr := fmt.Sprintf(":%d", serv.port)
    ln, err := net.Listen("tcp", addr)
    if err != nil {
        return err
    }
    serv.listener = ln
    serv.running = true
    log.Println("Service started and listening on port", serv.port)

    // start goroutine
    go serv.acceptConnections()

    return nil
}

// accept new connections from client callers until someone calls
func (serv *Service) acceptConnections() {
    defer serv.listener.Close()

    for {
        // accept connection
        conn, err := serv.listener.Accept()
        if err != nil {
            fmt.Println("Error accepting connection:", err)
            return
        }
		ls := NewLeakySocket(conn, serv.lossy, serv.delayed)
		serv.LeakySocket = ls
        // process connection logic in a new goroutine
        go serv.handleConnection(ls)
    }
}

// handle each client connection
// 1. receive a byte-string from the client
// 2. decode the byte-string into a RequestMsg
// 3. check if the Service's interface's Type includes a method with the given name
// 4. check if the method's type is correct
// 5. invoke the method
// 6. serialize the reply
// 7. send the reply message
// 8. close the connection
// @param ls -- the LeakySocket
func (serv *Service) handleConnection(ls *LeakySocket) {
	// receive a byte-string on `ls` using `ls.RecvObject()`
	buf, err := ls.RecvObject()
	if err != nil {
		log.Println("Error receiving object:", err)
		return
	}
	log.Printf("received:%x\n", buf)

	// decoding the byte-string
	var request RequestMsg
	dec := gob.NewDecoder(bytes.NewReader(buf))
	errDecDecode := dec.Decode(&request)
	if errDecDecode != nil {
		log.Println("Error decoding request:", errDecDecode)
		return
	}
	log.Println("decoded request:", request.Method, request.Args)
	// deserialize the arguments
	decodedArgs := deserializeReflectValues(request.Args)

	// check to see if the service interface's Type includes a method
	// with the given name
	method := serv.RemoteObject.MethodByName(request.Method)
	if !method.IsValid() {
		log.Println("Method not found:", request.Method)
		replyErr := ReplyMsg{
			Success: 404,
			Reply: nil,
			Err: RemoteObjectError{Err: "method not found"},
		}
		replyBytes := encodeReply(replyErr)
		realtimeSend(ls, replyBytes)
		return
	}
	log.Println("Method found:", request.Method)

	// check if the method's type is correct
	if method.Type().NumIn() != len(decodedArgs) {
		log.Println("Method's input parameters number is not correct")
		replyErr := ReplyMsg{
			Success: 405,
			Reply: nil,
			Err: RemoteObjectError{Err: "method's input parameters number is not correct"},
		}
		replyBytes := encodeReply(replyErr)
		realtimeSend(ls, replyBytes)
		return
	}
	for i:=0; i<method.Type().NumIn(); i++ {
		if method.Type().In(i) != decodedArgs[i].Type() {
			log.Println("Method's input parameters type is not correct")
			replyErr := ReplyMsg{
				Success: 500,
				Reply: nil,
				Err: RemoteObjectError{Err: "method's input parameters type is not correct"},
			}
			replyBytes := encodeReply(replyErr)
			realtimeSend(ls, replyBytes)
			return
		}
	}

	// invoke method
	ans := method.Call(decodedArgs)
	serializeAns := serializeReflectValues(ans)
	reply := ReplyMsg{
		Success: 200,
		Reply: serializeAns,
		Err: RemoteObjectError{},
	}

	replyBytes := encodeReply(reply)
	log.Printf("reply send:%x\n", replyBytes)
	
	// send the reply message
	realtimeSend(ls, replyBytes)

	// close the connection	
	// ls.Close()
}

func (serv *Service) GetCount() int {
	// TODO: return the total number of remote calls served successfully by this Service
	return 0
}


// return the current status of the Service
func (serv *Service) IsRunning() bool {
	// TODO: return a boolean value indicating whether the Service is running
	return serv.running
}

// stop the Service's tcp listening connection, update the Service
func (serv *Service) Stop() {
	// TODO: stop the Service, change state accordingly, clean up any resources
	serv.running = false
    serv.listener.Close()
}

// StubFactory -- make a client-side stub
//
// StubFactory uses reflection to populate the interface functions to create the
// caller's stub interface. Only works if all functions are exported/public.
// Once created, the interface masks remote calls to a Service that hosts the
// object instance that the functions are invoked on.  The network address of the
// remote Service must be provided with the stub is created, and it may not change later.
// @param ifc -- the interface to be populated with stub functions
// @param adr -- the network address of the remote Service
// @param lossy -- boolean value indicating whether the connection should experience packet loss
// @param delayed -- boolean value indicating whether the connection should experience transmission delay
// @return a local error if the interface is not a pointer to a struct
func StubFactory(ifc interface{}, adr string, lossy bool, delayed bool) error {
	// Check if ifc equals nil
	if ifc == nil {
		return errors.New("function struct is nil")
	}
	// Get the reflected type and value of the interface
	ifcType := reflect.TypeOf(ifc)
	ifcValue := reflect.ValueOf(ifc)
	// Check if the interface is a interface to a struct
	if ifcType.Kind() != reflect.Ptr || ifcType.Elem().Kind() != reflect.Struct {
		return errors.New("interface must be a pointer to a struct")
	}
	// Get the reflected type and value of the struct
	ifcElem := ifcType.Elem()
	ifcValueElem := ifcValue.Elem()

	for i := 0; i < ifcElem.NumField(); i++ {
		method := ifcElem.Field(i)
		// check if ifc's Method supports RemoteObjectError
		if method.Type.Out(method.Type.NumOut()-1) != reflect.TypeOf(RemoteObjectError{}) {
			return errors.New("method does not follow remote call convention")
		}

		paramsIn, paramsOut := putInParams(method.Type)
		zeroResult := make([]reflect.Value, len(paramsOut))
		for i:=0; i<len(paramsOut); i++ {
			zeroResult[i] = reflect.Zero(paramsOut[i])
		}
		addFuncType := reflect.FuncOf(
			paramsIn,
			paramsOut,
			false,
		)
		// Create a proxy function
		proxyFunc := func(args []reflect.Value) []reflect.Value {
			// Create a TCP connection to the Service
			conn, err := net.DialTimeout("tcp", adr, 5 * time.Second)
			errorDialObject := RemoteObjectError{Err: "failed to connect to Service"}
			if err != nil {
				zeroResult[len(paramsOut)-1] = reflect.ValueOf(errorDialObject)
				return zeroResult
			}
			// defer conn.Close()
			// Wrap the connection in a LeakySocket
			ls := NewLeakySocket(conn, lossy, delayed)
			log.Println("dial success to addr:", ls)
	
			// serialize the arguments
			serializedArgs := serializeReflectValues(args)
	
			// Create a RequestMsg
			request := RequestMsg{
				Method: method.Name,
				Args: serializedArgs,
			}
			// Encode the request message into a byte-string
			// using gob
			var buf bytes.Buffer
			enc := gob.NewEncoder(&buf)
			errEncEncode := enc.Encode(request)
			if errEncEncode != nil {
				log.Println("Error encoding request:", errEncEncode)
			}
			log.Printf("request:%x\n", buf.Bytes())
	
			// Send the request message
			realtimeSend(ls, buf.Bytes())
	
			// Receive the response message
			bufReply, err := ls.RecvObject()
			if err != nil {
				log.Println("Error receiving object:", err)
			}
	
			// Decode the response message
			var reply ReplyMsg
			dec := gob.NewDecoder(bytes.NewReader(bufReply))
			errDecDecode := dec.Decode(&reply)
			if errDecDecode != nil {
				log.Println("Error decoding reply:", errDecDecode)
			}
			log.Println("decoded reply:", reply.Success, reply.Reply, reply.Err)
			if reply.Success == 404 {
				log.Println("Error in finding method:", reply.Err.Err)
				zeroResult[len(paramsOut)-1] = reflect.ValueOf(reply.Err)
				return zeroResult
			} else if reply.Success == 500 {
				log.Println("Error in argument type:", reply.Err.Err)
				zeroResult[len(paramsOut)-1] = reflect.ValueOf(reply.Err)
				return zeroResult
			} else if reply.Success == 405 {
				log.Println("Error in argument number:", reply.Err.Err)
				zeroResult[len(paramsOut)-1] = reflect.ValueOf(reply.Err)
				return zeroResult
			}

			// deserialize the reply
			deserializedReply := deserializeReflectValues(reply.Reply)
			
			// check if the reply matches the expected return types
			if len(deserializedReply) != len(paramsOut) {
				log.Println("Error in processing reply: reply length not correct")
				errorReplyLengthObject := RemoteObjectError{Err: "failed to connect to Service"}
				return []reflect.Value{reflect.ValueOf(0), reflect.ValueOf(errorReplyLengthObject)}
			}
			for i:=0; i<len(deserializedReply); i++ {
				if deserializedReply[i].Type() != paramsOut[i] {
					log.Println("Error in processing reply: reply type not correct")
					zeroResult[len(paramsOut)-1] = reflect.ValueOf(reply.Err)
					return zeroResult
				}
			}

			// return the reply
			return deserializedReply
		}
		addFuncValue := reflect.MakeFunc(addFuncType, proxyFunc)
		ifcValueElem.FieldByName(method.Name).Set(addFuncValue)
	}

	return nil
}

// serialize the arguments
// using reflection
// @param values -- the arguments of a method
// @return the serialized arguments
func serializeReflectValues(values []reflect.Value) []interface{} {
	serialized := make([]interface{}, 0, len(values))
	for _, value := range values {
		serialized = append(serialized, value.Interface())
	}
	return serialized
}

// deserialize the reply
// using reflection
// @param values -- the reply argument of a remote method
// @return the deserialized reply
func deserializeReflectValues(values []interface{}) []reflect.Value {
	deserialized := make([]reflect.Value, 0, len(values))
	for _, value := range values {
		deserialized = append(deserialized, reflect.ValueOf(value))
	}
	return deserialized
}

// put the input and output parameters into slices
// using reflection
// @param params -- params of a method
// @return the input and output parameters types of a method
func putInParams(params reflect.Type) ([]reflect.Type, []reflect.Type) {
	paramsInNum := params.NumIn()
	paramsOutNum := params.NumOut()
	paramsIn := make([]reflect.Type, paramsInNum)
	paramsOut := make([]reflect.Type, paramsOutNum)
	for i:=0; i<paramsInNum; i++ {
		paramsIn[i] = params.In(i)
	}
	for i:=0; i<paramsOutNum; i++ {
		paramsOut[i] = params.Out(i)
	}
	return paramsIn, paramsOut
}

// encode the reply message into a byte-string
// using gob
// @param reply -- the reply message
// @return the encoded reply message
func encodeReply(reply ReplyMsg) []byte {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	errEncEncode := enc.Encode(reply)
	if errEncEncode != nil {
		log.Println("Error encoding reply:", errEncEncode)
	}
	return buf.Bytes()
}

// send the reply message
// using a LeakySocket
// @param ls -- the LeakySocket
// @param buf -- the reply message
// @return the success of sending the reply message
func realtimeSend(ls *LeakySocket, buf []byte) {
	for {
		success, err := ls.SendObject(buf)
		if success {
			log.Println("send success")
			break
		} else {
			log.Println("send failed:", err)
		}
	}
}

// NewService -- server side stub/skeleton
func init() {
    gob.Register(RemoteObjectError{})
}