// Package grpcurl provides the core functionality exposed by the grpcurl command, for
// dynamically connecting to a server, using the reflection service to inspect the server,
// and invoking RPCs. The grpcurl command-line tool constructs a DescriptorSource, based
// on the command-line parameters, and supplies an InvocationEventHandler to supply request
// data (which can come from command-line args or the process's stdin) and to log the
// events (to the process's stdout).
package grpcurl

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	descpb "github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoparse"
	"github.com/jhump/protoreflect/desc/protoprint"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/dynamic/grpcdynamic"
	"github.com/jhump/protoreflect/grpcreflect"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ErrReflectionNotSupported is returned by DescriptorSource operations that
// rely on interacting with the reflection service when the source does not
// actually expose the reflection service. When this occurs, an alternate source
// (like file descriptor sets) must be used.
var ErrReflectionNotSupported = errors.New("server does not support the reflection API")

// DescriptorSource is a source of protobuf descriptor information. It can be backed by a FileDescriptorSet
// proto (like a file generated by protoc) or a remote server that supports the reflection API.
type DescriptorSource interface {
	// ListServices returns a list of fully-qualified service names. It will be all services in a set of
	// descriptor files or the set of all services exposed by a gRPC server.
	ListServices() ([]string, error)
	// FindSymbol returns a descriptor for the given fully-qualified symbol name.
	FindSymbol(fullyQualifiedName string) (desc.Descriptor, error)
	// AllExtensionsForType returns all known extension fields that extend the given message type name.
	AllExtensionsForType(typeName string) ([]*desc.FieldDescriptor, error)
}

// DescriptorSourceFromProtoSets creates a DescriptorSource that is backed by the named files, whose contents
// are encoded FileDescriptorSet protos.
func DescriptorSourceFromProtoSets(fileNames ...string) (DescriptorSource, error) {
	files := &descpb.FileDescriptorSet{}
	for _, fileName := range fileNames {
		b, err := ioutil.ReadFile(fileName)
		if err != nil {
			return nil, fmt.Errorf("could not load protoset file %q: %v", fileName, err)
		}
		var fs descpb.FileDescriptorSet
		err = proto.Unmarshal(b, &fs)
		if err != nil {
			return nil, fmt.Errorf("could not parse contents of protoset file %q: %v", fileName, err)
		}
		files.File = append(files.File, fs.File...)
	}
	return DescriptorSourceFromFileDescriptorSet(files)
}

// DescriptorSourceFromProtoFiles creates a DescriptorSource that is backed by the named files,
// whose contents are Protocol Buffer source files. The given importPaths are used to locate
// any imported files.
func DescriptorSourceFromProtoFiles(importPaths []string, fileNames ...string) (DescriptorSource, error) {
	p := protoparse.Parser{
		ImportPaths:      importPaths,
		InferImportPaths: len(importPaths) == 0,
	}
	fds, err := p.ParseFiles(fileNames...)
	if err != nil {
		return nil, fmt.Errorf("could not parse given files: %v", err)
	}
	return DescriptorSourceFromFileDescriptors(fds...)
}

// DescriptorSourceFromFileDescriptorSet creates a DescriptorSource that is backed by the FileDescriptorSet.
func DescriptorSourceFromFileDescriptorSet(files *descpb.FileDescriptorSet) (DescriptorSource, error) {
	unresolved := map[string]*descpb.FileDescriptorProto{}
	for _, fd := range files.File {
		unresolved[fd.GetName()] = fd
	}
	resolved := map[string]*desc.FileDescriptor{}
	for _, fd := range files.File {
		_, err := resolveFileDescriptor(unresolved, resolved, fd.GetName())
		if err != nil {
			return nil, err
		}
	}
	return &fileSource{files: resolved}, nil
}

func resolveFileDescriptor(unresolved map[string]*descpb.FileDescriptorProto, resolved map[string]*desc.FileDescriptor, filename string) (*desc.FileDescriptor, error) {
	if r, ok := resolved[filename]; ok {
		return r, nil
	}
	fd, ok := unresolved[filename]
	if !ok {
		return nil, fmt.Errorf("no descriptor found for %q", filename)
	}
	deps := make([]*desc.FileDescriptor, 0, len(fd.GetDependency()))
	for _, dep := range fd.GetDependency() {
		depFd, err := resolveFileDescriptor(unresolved, resolved, dep)
		if err != nil {
			return nil, err
		}
		deps = append(deps, depFd)
	}
	result, err := desc.CreateFileDescriptor(fd, deps...)
	if err != nil {
		return nil, err
	}
	resolved[filename] = result
	return result, nil
}

// DescriptorSourceFromFileDescriptorSet creates a DescriptorSource that is backed by the given
// file descriptors
func DescriptorSourceFromFileDescriptors(files ...*desc.FileDescriptor) (DescriptorSource, error) {
	fds := map[string]*desc.FileDescriptor{}
	for _, fd := range files {
		if err := addFile(fd, fds); err != nil {
			return nil, err
		}
	}
	return &fileSource{files: fds}, nil
}

func addFile(fd *desc.FileDescriptor, fds map[string]*desc.FileDescriptor) error {
	name := fd.GetName()
	if existing, ok := fds[name]; ok {
		// already added this file
		if existing != fd {
			// doh! duplicate files provided
			return fmt.Errorf("given files include multiple copies of %q", name)
		}
		return nil
	}
	fds[name] = fd
	for _, dep := range fd.GetDependencies() {
		if err := addFile(dep, fds); err != nil {
			return err
		}
	}
	return nil
}

type fileSource struct {
	files  map[string]*desc.FileDescriptor
	er     *dynamic.ExtensionRegistry
	erInit sync.Once
}

func (fs *fileSource) ListServices() ([]string, error) {
	set := map[string]bool{}
	for _, fd := range fs.files {
		for _, svc := range fd.GetServices() {
			set[svc.GetFullyQualifiedName()] = true
		}
	}
	sl := make([]string, 0, len(set))
	for svc := range set {
		sl = append(sl, svc)
	}
	return sl, nil
}

// GetAllFiles returns all of the underlying file descriptors. This is
// more thorough and more efficient than the fallback strategy used by
// the GetAllFiles package method, for enumerating all files from a
// descriptor source.
func (fs *fileSource) GetAllFiles() ([]*desc.FileDescriptor, error) {
	files := make([]*desc.FileDescriptor, len(fs.files))
	i := 0
	for _, fd := range fs.files {
		files[i] = fd
		i++
	}
	return files, nil
}

func (fs *fileSource) FindSymbol(fullyQualifiedName string) (desc.Descriptor, error) {
	for _, fd := range fs.files {
		if dsc := fd.FindSymbol(fullyQualifiedName); dsc != nil {
			return dsc, nil
		}
	}
	return nil, notFound("Symbol", fullyQualifiedName)
}

func (fs *fileSource) AllExtensionsForType(typeName string) ([]*desc.FieldDescriptor, error) {
	fs.erInit.Do(func() {
		fs.er = &dynamic.ExtensionRegistry{}
		for _, fd := range fs.files {
			fs.er.AddExtensionsFromFile(fd)
		}
	})
	return fs.er.AllExtensionsForType(typeName), nil
}

// DescriptorSourceFromServer creates a DescriptorSource that uses the given gRPC reflection client
// to interrogate a server for descriptor information. If the server does not support the reflection
// API then the various DescriptorSource methods will return ErrReflectionNotSupported
func DescriptorSourceFromServer(ctx context.Context, refClient *grpcreflect.Client) DescriptorSource {
	return serverSource{client: refClient}
}

type serverSource struct {
	client *grpcreflect.Client
}

func (ss serverSource) ListServices() ([]string, error) {
	svcs, err := ss.client.ListServices()
	return svcs, reflectionSupport(err)
}

func (ss serverSource) FindSymbol(fullyQualifiedName string) (desc.Descriptor, error) {
	file, err := ss.client.FileContainingSymbol(fullyQualifiedName)
	if err != nil {
		return nil, reflectionSupport(err)
	}
	d := file.FindSymbol(fullyQualifiedName)
	if d == nil {
		return nil, notFound("Symbol", fullyQualifiedName)
	}
	return d, nil
}

func (ss serverSource) AllExtensionsForType(typeName string) ([]*desc.FieldDescriptor, error) {
	var exts []*desc.FieldDescriptor
	nums, err := ss.client.AllExtensionNumbersForType(typeName)
	if err != nil {
		return nil, reflectionSupport(err)
	}
	for _, fieldNum := range nums {
		ext, err := ss.client.ResolveExtension(typeName, fieldNum)
		if err != nil {
			return nil, reflectionSupport(err)
		}
		exts = append(exts, ext)
	}
	return exts, nil
}

func reflectionSupport(err error) error {
	if err == nil {
		return nil
	}
	if stat, ok := status.FromError(err); ok && stat.Code() == codes.Unimplemented {
		return ErrReflectionNotSupported
	}
	return err
}

// ListServices uses the given descriptor source to return a sorted list of fully-qualified
// service names.
func ListServices(source DescriptorSource) ([]string, error) {
	svcs, err := source.ListServices()
	if err != nil {
		return nil, err
	}
	sort.Strings(svcs)
	return svcs, nil
}

type sourceWithFiles interface {
	GetAllFiles() ([]*desc.FileDescriptor, error)
}

var _ sourceWithFiles = (*fileSource)(nil)

// GetAllFiles uses the given descriptor source to return a list of file descriptors.
func GetAllFiles(source DescriptorSource) ([]*desc.FileDescriptor, error) {
	var files []*desc.FileDescriptor
	srcFiles, ok := source.(sourceWithFiles)

	if ok {
		var err error
		files, err = srcFiles.GetAllFiles()
		if err != nil {
			return nil, err
		}
	} else {
		// Source does not implement GetAllFiles method, so use ListServices
		// and grab files from there.
		allFiles := map[string]*desc.FileDescriptor{}
		svcNames, err := source.ListServices()
		if err != nil {
			return nil, err
		}
		for _, name := range svcNames {
			d, err := source.FindSymbol(name)
			if err != nil {
				return nil, err
			}
			addAllFilesToSet(d.GetFile(), allFiles)
		}
		files = make([]*desc.FileDescriptor, len(allFiles))
		i := 0
		for _, fd := range allFiles {
			files[i] = fd
			i++
		}
	}

	sort.Sort(filesByName(files))
	return files, nil
}

type filesByName []*desc.FileDescriptor

func (f filesByName) Len() int {
	return len(f)
}

func (f filesByName) Less(i, j int) bool {
	return f[i].GetName() < f[j].GetName()
}

func (f filesByName) Swap(i, j int) {
	f[i], f[j] = f[j], f[i]
}

func addAllFilesToSet(fd *desc.FileDescriptor, all map[string]*desc.FileDescriptor) {
	if _, ok := all[fd.GetName()]; ok {
		// already added
		return
	}
	all[fd.GetName()] = fd
	for _, dep := range fd.GetDependencies() {
		addAllFilesToSet(dep, all)
	}
}

// ListMethods uses the given descriptor source to return a sorted list of method names
// for the specified fully-qualified service name.
func ListMethods(source DescriptorSource, serviceName string) ([]string, error) {
	dsc, err := source.FindSymbol(serviceName)
	if err != nil {
		return nil, err
	}
	if sd, ok := dsc.(*desc.ServiceDescriptor); !ok {
		return nil, notFound("Service", serviceName)
	} else {
		methods := make([]string, 0, len(sd.GetMethods()))
		for _, method := range sd.GetMethods() {
			methods = append(methods, method.GetName())
		}
		sort.Strings(methods)
		return methods, nil
	}
}

type notFoundError string

func notFound(kind, name string) error {
	return notFoundError(fmt.Sprintf("%s not found: %s", kind, name))
}

func (e notFoundError) Error() string {
	return string(e)
}

func isNotFoundError(err error) bool {
	if grpcreflect.IsElementNotFoundError(err) {
		return true
	}
	_, ok := err.(notFoundError)
	return ok
}

// InvocationEventHandler is a bag of callbacks for handling events that occur in the course
// of invoking an RPC. The handler also provides request data that is sent. The callbacks are
// generally called in the order they are listed below.
type InvocationEventHandler interface {
	// OnResolveMethod is called with a descriptor of the method that is being invoked.
	OnResolveMethod(*desc.MethodDescriptor)
	// OnSendHeaders is called with the request metadata that is being sent.
	OnSendHeaders(metadata.MD)
	// OnReceiveHeaders is called when response headers have been received.
	OnReceiveHeaders(metadata.MD)
	// OnReceiveResponse is called for each response message received.
	OnReceiveResponse(proto.Message)
	// OnReceiveTrailers is called when response trailers and final RPC status have been received.
	OnReceiveTrailers(*status.Status, metadata.MD)
}

// RequestMessageSupplier is a function that is called to retrieve request
// messages for a GRPC operation. This type is deprecated and will be removed in
// a future release.
//
// Deprecated: This is only used with the deprecated InvokeRpc. Instead, use
// RequestSupplier with InvokeRPC.
type RequestMessageSupplier func() ([]byte, error)

// InvokeRpc uses the given gRPC connection to invoke the given method. This function is deprecated
// and will be removed in a future release. It just delegates to the similarly named InvokeRPC
// method, whose signature is only slightly different.
//
// Deprecated: use InvokeRPC instead.
func InvokeRpc(ctx context.Context, source DescriptorSource, cc *grpc.ClientConn, methodName string,
	headers []string, handler InvocationEventHandler, requestData RequestMessageSupplier) error {

	return InvokeRPC(ctx, source, cc, methodName, headers, handler, func(m proto.Message) error {
		// New function is almost identical, but the request supplier function works differently.
		// So we adapt the logic here to maintain compatibility.
		data, err := requestData()
		if err != nil {
			return err
		}
		return jsonpb.Unmarshal(bytes.NewReader(data), m)
	})
}

// RequestSupplier is a function that is called to populate messages for a gRPC operation. The
// function should populate the given message or return a non-nil error. If the supplier has no
// more messages, it should return io.EOF. When it returns io.EOF, it should not in any way
// modify the given message argument.
type RequestSupplier func(proto.Message) error

// InvokeRPC uses the given gRPC channel to invoke the given method. The given descriptor source
// is used to determine the type of method and the type of request and response message. The given
// headers are sent as request metadata. Methods on the given event handler are called as the
// invocation proceeds.
//
// The given requestData function supplies the actual data to send. It should return io.EOF when
// there is no more request data. If the method being invoked is a unary or server-streaming RPC
// (e.g. exactly one request message) and there is no request data (e.g. the first invocation of
// the function returns io.EOF), then an empty request message is sent.
//
// If the requestData function and the given event handler coordinate or share any state, they should
// be thread-safe. This is because the requestData function may be called from a different goroutine
// than the one invoking event callbacks. (This only happens for bi-directional streaming RPCs, where
// one goroutine sends request messages and another consumes the response messages).
func InvokeRPC(ctx context.Context, source DescriptorSource, ch grpcdynamic.Channel, methodName string,
	headers []string, handler InvocationEventHandler, requestData RequestSupplier) error {

	md := MetadataFromHeaders(headers)

	svc, mth := parseSymbol(methodName)
	if svc == "" || mth == "" {
		return fmt.Errorf("given method name %q is not in expected format: 'service/method' or 'service.method'", methodName)
	}
	dsc, err := source.FindSymbol(svc)
	if err != nil {
		if isNotFoundError(err) {
			return fmt.Errorf("target server does not expose service %q", svc)
		}
		return fmt.Errorf("failed to query for service descriptor %q: %v", svc, err)
	}
	sd, ok := dsc.(*desc.ServiceDescriptor)
	if !ok {
		return fmt.Errorf("target server does not expose service %q", svc)
	}
	mtd := sd.FindMethodByName(mth)
	if mtd == nil {
		return fmt.Errorf("service %q does not include a method named %q", svc, mth)
	}

	handler.OnResolveMethod(mtd)

	// we also download any applicable extensions so we can provide full support for parsing user-provided data
	var ext dynamic.ExtensionRegistry
	alreadyFetched := map[string]bool{}
	if err = fetchAllExtensions(source, &ext, mtd.GetInputType(), alreadyFetched); err != nil {
		return fmt.Errorf("error resolving server extensions for message %s: %v", mtd.GetInputType().GetFullyQualifiedName(), err)
	}
	if err = fetchAllExtensions(source, &ext, mtd.GetOutputType(), alreadyFetched); err != nil {
		return fmt.Errorf("error resolving server extensions for message %s: %v", mtd.GetOutputType().GetFullyQualifiedName(), err)
	}

	msgFactory := dynamic.NewMessageFactoryWithExtensionRegistry(&ext)
	req := msgFactory.NewMessage(mtd.GetInputType())

	handler.OnSendHeaders(md)
	ctx = metadata.NewOutgoingContext(ctx, md)

	stub := grpcdynamic.NewStubWithMessageFactory(ch, msgFactory)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if mtd.IsClientStreaming() && mtd.IsServerStreaming() {
		return invokeBidi(ctx, stub, mtd, handler, requestData, req)
	} else if mtd.IsClientStreaming() {
		return invokeClientStream(ctx, stub, mtd, handler, requestData, req)
	} else if mtd.IsServerStreaming() {
		return invokeServerStream(ctx, stub, mtd, handler, requestData, req)
	} else {
		return invokeUnary(ctx, stub, mtd, handler, requestData, req)
	}
}

func invokeUnary(ctx context.Context, stub grpcdynamic.Stub, md *desc.MethodDescriptor, handler InvocationEventHandler,
	requestData RequestSupplier, req proto.Message) error {

	err := requestData(req)
	if err != nil && err != io.EOF {
		return fmt.Errorf("error getting request data: %v", err)
	}
	if err != io.EOF {
		// verify there is no second message, which is a usage error
		err := requestData(req)
		if err == nil {
			return fmt.Errorf("method %q is a unary RPC, but request data contained more than 1 message", md.GetFullyQualifiedName())
		} else if err != io.EOF {
			return fmt.Errorf("error getting request data: %v", err)
		}
	}

	// Now we can actually invoke the RPC!
	var respHeaders metadata.MD
	var respTrailers metadata.MD
	resp, err := stub.InvokeRpc(ctx, md, req, grpc.Trailer(&respTrailers), grpc.Header(&respHeaders))

	stat, ok := status.FromError(err)
	if !ok {
		// Error codes sent from the server will get printed differently below.
		// So just bail for other kinds of errors here.
		return fmt.Errorf("grpc call for %q failed: %v", md.GetFullyQualifiedName(), err)
	}

	handler.OnReceiveHeaders(respHeaders)

	if stat.Code() == codes.OK {
		handler.OnReceiveResponse(resp)
	}

	handler.OnReceiveTrailers(stat, respTrailers)

	return nil
}

func invokeClientStream(ctx context.Context, stub grpcdynamic.Stub, md *desc.MethodDescriptor, handler InvocationEventHandler,
	requestData RequestSupplier, req proto.Message) error {

	// invoke the RPC!
	str, err := stub.InvokeRpcClientStream(ctx, md)

	// Upload each request message in the stream
	var resp proto.Message
	for err == nil {
		err = requestData(req)
		if err == io.EOF {
			resp, err = str.CloseAndReceive()
			break
		}
		if err != nil {
			return fmt.Errorf("error getting request data: %v", err)
		}

		err = str.SendMsg(req)
		if err == io.EOF {
			// We get EOF on send if the server says "go away"
			// We have to use CloseAndReceive to get the actual code
			resp, err = str.CloseAndReceive()
			break
		}

		req.Reset()
	}

	// finally, process response data
	stat, ok := status.FromError(err)
	if !ok {
		// Error codes sent from the server will get printed differently below.
		// So just bail for other kinds of errors here.
		return fmt.Errorf("grpc call for %q failed: %v", md.GetFullyQualifiedName(), err)
	}

	if respHeaders, err := str.Header(); err == nil {
		handler.OnReceiveHeaders(respHeaders)
	}

	if stat.Code() == codes.OK {
		handler.OnReceiveResponse(resp)
	}

	handler.OnReceiveTrailers(stat, str.Trailer())

	return nil
}

func invokeServerStream(ctx context.Context, stub grpcdynamic.Stub, md *desc.MethodDescriptor, handler InvocationEventHandler,
	requestData RequestSupplier, req proto.Message) error {

	err := requestData(req)
	if err != nil && err != io.EOF {
		return fmt.Errorf("error getting request data: %v", err)
	}
	if err != io.EOF {
		// verify there is no second message, which is a usage error
		err := requestData(req)
		if err == nil {
			return fmt.Errorf("method %q is a server-streaming RPC, but request data contained more than 1 message", md.GetFullyQualifiedName())
		} else if err != io.EOF {
			return fmt.Errorf("error getting request data: %v", err)
		}
	}

	// Now we can actually invoke the RPC!
	str, err := stub.InvokeRpcServerStream(ctx, md, req)

	if respHeaders, err := str.Header(); err == nil {
		handler.OnReceiveHeaders(respHeaders)
	}

	// Download each response message
	for err == nil {
		var resp proto.Message
		resp, err = str.RecvMsg()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
		handler.OnReceiveResponse(resp)
	}

	stat, ok := status.FromError(err)
	if !ok {
		// Error codes sent from the server will get printed differently below.
		// So just bail for other kinds of errors here.
		return fmt.Errorf("grpc call for %q failed: %v", md.GetFullyQualifiedName(), err)
	}

	handler.OnReceiveTrailers(stat, str.Trailer())

	return nil
}

func invokeBidi(ctx context.Context, stub grpcdynamic.Stub, md *desc.MethodDescriptor, handler InvocationEventHandler,
	requestData RequestSupplier, req proto.Message) error {

	// invoke the RPC!
	str, err := stub.InvokeRpcBidiStream(ctx, md)

	var wg sync.WaitGroup
	var sendErr atomic.Value

	defer wg.Wait()

	if err == nil {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Concurrently upload each request message in the stream
			var err error
			for err == nil {
				err = requestData(req)

				if err == io.EOF {
					err = str.CloseSend()
					break
				}
				if err != nil {
					err = fmt.Errorf("error getting request data: %v", err)
					break
				}

				err = str.SendMsg(req)

				req.Reset()
			}

			if err != nil {
				sendErr.Store(err)
			}
		}()
	}

	if respHeaders, err := str.Header(); err == nil {
		handler.OnReceiveHeaders(respHeaders)
	}

	// Download each response message
	for err == nil {
		var resp proto.Message
		resp, err = str.RecvMsg()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
		handler.OnReceiveResponse(resp)
	}

	if se, ok := sendErr.Load().(error); ok && se != io.EOF {
		err = se
	}

	stat, ok := status.FromError(err)
	if !ok {
		// Error codes sent from the server will get printed differently below.
		// So just bail for other kinds of errors here.
		return fmt.Errorf("grpc call for %q failed: %v", md.GetFullyQualifiedName(), err)
	}

	handler.OnReceiveTrailers(stat, str.Trailer())

	return nil
}

// MetadataFromHeaders converts a list of header strings (each string in
// "Header-Name: Header-Value" form) into metadata. If a string has a header
// name without a value (e.g. does not contain a colon), the value is assumed
// to be blank. Binary headers (those whose names end in "-bin") should be
// base64-encoded. But if they cannot be base64-decoded, they will be assumed to
// be in raw form and used as is.
func MetadataFromHeaders(headers []string) metadata.MD {
	md := make(metadata.MD)
	for _, part := range headers {
		if part != "" {
			pieces := strings.SplitN(part, ":", 2)
			if len(pieces) == 1 {
				pieces = append(pieces, "") // if no value was specified, just make it "" (maybe the header value doesn't matter)
			}
			headerName := strings.ToLower(strings.TrimSpace(pieces[0]))
			val := strings.TrimSpace(pieces[1])
			if strings.HasSuffix(headerName, "-bin") {
				if v, err := decode(val); err == nil {
					val = v
				}
			}
			md[headerName] = append(md[headerName], val)
		}
	}
	return md
}

var base64Codecs = []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding}

func decode(val string) (string, error) {
	var firstErr error
	var b []byte
	// we are lenient and can accept any of the flavors of base64 encoding
	for _, d := range base64Codecs {
		var err error
		b, err = d.DecodeString(val)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return string(b), nil
	}
	return "", firstErr
}

func parseSymbol(svcAndMethod string) (string, string) {
	pos := strings.LastIndex(svcAndMethod, "/")
	if pos < 0 {
		pos = strings.LastIndex(svcAndMethod, ".")
		if pos < 0 {
			return "", ""
		}
	}
	return svcAndMethod[:pos], svcAndMethod[pos+1:]
}

// MetadataToString returns a string representation of the given metadata, for
// displaying to users.
func MetadataToString(md metadata.MD) string {
	if len(md) == 0 {
		return "(empty)"
	}

	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	first := true
	for _, k := range keys {
		vs := md[k]
		for _, v := range vs {
			if first {
				first = false
			} else {
				b.WriteString("\n")
			}
			b.WriteString(k)
			b.WriteString(": ")
			if strings.HasSuffix(k, "-bin") {
				v = base64.StdEncoding.EncodeToString([]byte(v))
			}
			b.WriteString(v)
		}
	}
	return b.String()
}

var printer = &protoprint.Printer{
	Compact:                  true,
	OmitComments:             protoprint.CommentsNonDoc,
	SortElements:             true,
	ForceFullyQualifiedNames: true,
}

// GetDescriptorText returns a string representation of the given descriptor.
// This returns a snippet of proto source that describes the given element.
func GetDescriptorText(dsc desc.Descriptor, _ DescriptorSource) (string, error) {
	// Note: DescriptorSource is not used, but remains an argument for backwards
	// compatibility with previous implementation.
	txt, err := printer.PrintProtoToString(dsc)
	if err != nil {
		return "", err
	}
	// callers don't expect trailing newlines
	if txt[len(txt)-1] == '\n' {
		txt = txt[:len(txt)-1]
	}
	return txt, nil
}

// EnsureExtensions uses the given descriptor source to download extensions for
// the given message. It returns a copy of the given message, but as a dynamic
// message that knows about all extensions known to the given descriptor source.
func EnsureExtensions(source DescriptorSource, msg proto.Message) proto.Message {
	// load any server extensions so we can properly describe custom options
	dsc, err := desc.LoadMessageDescriptorForMessage(msg)
	if err != nil {
		return msg
	}

	var ext dynamic.ExtensionRegistry
	if err = fetchAllExtensions(source, &ext, dsc, map[string]bool{}); err != nil {
		return msg
	}

	// convert message into dynamic message that knows about applicable extensions
	// (that way we can show meaningful info for custom options instead of printing as unknown)
	msgFactory := dynamic.NewMessageFactoryWithExtensionRegistry(&ext)
	dm, err := fullyConvertToDynamic(msgFactory, msg)
	if err != nil {
		return msg
	}
	return dm
}

// fetchAllExtensions recursively fetches from the server extensions for the given message type as well as
// for all message types of nested fields. The extensions are added to the given dynamic registry of extensions
// so that all server-known extensions can be correctly parsed by grpcurl.
func fetchAllExtensions(source DescriptorSource, ext *dynamic.ExtensionRegistry, md *desc.MessageDescriptor, alreadyFetched map[string]bool) error {
	msgTypeName := md.GetFullyQualifiedName()
	if alreadyFetched[msgTypeName] {
		return nil
	}
	alreadyFetched[msgTypeName] = true
	if len(md.GetExtensionRanges()) > 0 {
		fds, err := source.AllExtensionsForType(msgTypeName)
		if err != nil {
			return fmt.Errorf("failed to query for extensions of type %s: %v", msgTypeName, err)
		}
		for _, fd := range fds {
			if err := ext.AddExtension(fd); err != nil {
				return fmt.Errorf("could not register extension %s of type %s: %v", fd.GetFullyQualifiedName(), msgTypeName, err)
			}
		}
	}
	// recursively fetch extensions for the types of any message fields
	for _, fd := range md.GetFields() {
		if fd.GetMessageType() != nil {
			err := fetchAllExtensions(source, ext, fd.GetMessageType(), alreadyFetched)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// fullConvertToDynamic attempts to convert the given message to a dynamic message as well
// as any nested messages it may contain as field values. If the given message factory has
// extensions registered that were not known when the given message was parsed, this effectively
// allows re-parsing to identify those extensions.
func fullyConvertToDynamic(msgFact *dynamic.MessageFactory, msg proto.Message) (proto.Message, error) {
	if _, ok := msg.(*dynamic.Message); ok {
		return msg, nil // already a dynamic message
	}
	md, err := desc.LoadMessageDescriptorForMessage(msg)
	if err != nil {
		return nil, err
	}
	newMsg := msgFact.NewMessage(md)
	dm, ok := newMsg.(*dynamic.Message)
	if !ok {
		// if message factory didn't produce a dynamic message, then we should leave msg as is
		return msg, nil
	}

	if err := dm.ConvertFrom(msg); err != nil {
		return nil, err
	}

	// recursively convert all field values, too
	for _, fd := range md.GetFields() {
		if fd.IsMap() {
			if fd.GetMapValueType().GetMessageType() != nil {
				m := dm.GetField(fd).(map[interface{}]interface{})
				for k, v := range m {
					// keys can't be nested messages; so we only need to recurse through map values, not keys
					newVal, err := fullyConvertToDynamic(msgFact, v.(proto.Message))
					if err != nil {
						return nil, err
					}
					dm.PutMapField(fd, k, newVal)
				}
			}
		} else if fd.IsRepeated() {
			if fd.GetMessageType() != nil {
				s := dm.GetField(fd).([]interface{})
				for i, e := range s {
					newVal, err := fullyConvertToDynamic(msgFact, e.(proto.Message))
					if err != nil {
						return nil, err
					}
					dm.SetRepeatedField(fd, i, newVal)
				}
			}
		} else {
			if fd.GetMessageType() != nil {
				v := dm.GetField(fd)
				newVal, err := fullyConvertToDynamic(msgFact, v.(proto.Message))
				if err != nil {
					return nil, err
				}
				dm.SetField(fd, newVal)
			}
		}
	}
	return dm, nil
}

// ClientTransportCredentials builds transport credentials for a gRPC client using the
// given properties. If cacertFile is blank, only standard trusted certs are used to
// verify the server certs. If clientCertFile is blank, the client will not use a client
// certificate. If clientCertFile is not blank then clientKeyFile must not be blank.
func ClientTransportCredentials(insecureSkipVerify bool, cacertFile, clientCertFile, clientKeyFile string) (credentials.TransportCredentials, error) {
	var tlsConf tls.Config

	if clientCertFile != "" {
		// Load the client certificates from disk
		certificate, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("could not load client key pair: %v", err)
		}
		tlsConf.Certificates = []tls.Certificate{certificate}
	}

	if insecureSkipVerify {
		tlsConf.InsecureSkipVerify = true
	} else if cacertFile != "" {
		// Create a certificate pool from the certificate authority
		certPool := x509.NewCertPool()
		ca, err := ioutil.ReadFile(cacertFile)
		if err != nil {
			return nil, fmt.Errorf("could not read ca certificate: %v", err)
		}

		// Append the certificates from the CA
		if ok := certPool.AppendCertsFromPEM(ca); !ok {
			return nil, errors.New("failed to append ca certs")
		}

		tlsConf.RootCAs = certPool
	}

	return credentials.NewTLS(&tlsConf), nil
}

// ServerTransportCredentials builds transport credentials for a gRPC server using the
// given properties. If cacertFile is blank, the server will not request client certs
// unless requireClientCerts is true. When requireClientCerts is false and cacertFile is
// not blank, the server will verify client certs when presented, but will not require
// client certs. The serverCertFile and serverKeyFile must both not be blank.
func ServerTransportCredentials(cacertFile, serverCertFile, serverKeyFile string, requireClientCerts bool) (credentials.TransportCredentials, error) {
	var tlsConf tls.Config

	// Load the server certificates from disk
	certificate, err := tls.LoadX509KeyPair(serverCertFile, serverKeyFile)
	if err != nil {
		return nil, fmt.Errorf("could not load key pair: %v", err)
	}
	tlsConf.Certificates = []tls.Certificate{certificate}

	if cacertFile != "" {
		// Create a certificate pool from the certificate authority
		certPool := x509.NewCertPool()
		ca, err := ioutil.ReadFile(cacertFile)
		if err != nil {
			return nil, fmt.Errorf("could not read ca certificate: %v", err)
		}

		// Append the certificates from the CA
		if ok := certPool.AppendCertsFromPEM(ca); !ok {
			return nil, errors.New("failed to append ca certs")
		}

		tlsConf.ClientCAs = certPool
	}

	if requireClientCerts {
		tlsConf.ClientAuth = tls.RequireAndVerifyClientCert
	} else if cacertFile != "" {
		tlsConf.ClientAuth = tls.VerifyClientCertIfGiven
	} else {
		tlsConf.ClientAuth = tls.NoClientCert
	}

	return credentials.NewTLS(&tlsConf), nil
}

// BlockingDial is a helper method to dial the given address, using optional TLS credentials,
// and blocking until the returned connection is ready. If the given credentials are nil, the
// connection will be insecure (plain-text).
func BlockingDial(ctx context.Context, network, address string, creds credentials.TransportCredentials, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	// grpc.Dial doesn't provide any information on permanent connection errors (like
	// TLS handshake failures). So in order to provide good error messages, we need a
	// custom dialer that can provide that info. That means we manage the TLS handshake.
	result := make(chan interface{}, 1)

	writeResult := func(res interface{}) {
		// non-blocking write: we only need the first result
		select {
		case result <- res:
		default:
		}
	}

	dialer := func(address string, timeout time.Duration) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		conn, err := (&net.Dialer{Cancel: ctx.Done()}).Dial(network, address)
		if err != nil {
			writeResult(err)
			return nil, err
		}
		if creds != nil {
			conn, _, err = creds.ClientHandshake(ctx, address, conn)
			if err != nil {
				writeResult(err)
				return nil, err
			}
		}
		return conn, nil
	}

	// Even with grpc.FailOnNonTempDialError, this call will usually timeout in
	// the face of TLS handshake errors. So we can't rely on grpc.WithBlock() to
	// know when we're done. So we run it in a goroutine and then use result
	// channel to either get the channel or fail-fast.
	go func() {
		opts = append(opts,
			grpc.WithBlock(),
			grpc.FailOnNonTempDialError(true),
			grpc.WithDialer(dialer),
			grpc.WithInsecure(), // we are handling TLS, so tell grpc not to
		)
		conn, err := grpc.DialContext(ctx, address, opts...)
		var res interface{}
		if err != nil {
			res = err
		} else {
			res = conn
		}
		writeResult(res)
	}()

	select {
	case res := <-result:
		if conn, ok := res.(*grpc.ClientConn); ok {
			return conn, nil
		}
		return nil, res.(error)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
