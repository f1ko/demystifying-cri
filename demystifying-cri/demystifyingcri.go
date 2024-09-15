package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	runtime "demystifying-cri/proto"

	"github.com/opencontainers/runtime-tools/generate"
	"google.golang.org/grpc"
)

// DemystifyingCRI implements both the RuntimeServiceServer and ImageServiceServer
type DemystifyingCRI struct {
	// Unimplemented provides default implementations for all methods
	// Otherwise every rpc in the proto must be implemented as a method
	runtime.UnimplementedRuntimeServiceServer
	runtime.UnimplementedImageServiceServer

	sandboxes  map[string]*runtime.PodSandbox // Quick way to store sandbox information
	containers map[string]*runtime.Container  // Quick way to store container information
	images     map[string]*runtime.Image      // Quick way to store image information

	runtimeRoot  string // Path to create containers at
	imageRoot    string // Path to download images to
	sandboxImage string // Image which is later used for sandboxes
}

// Implement RuntimeService methods

func (s *DemystifyingCRI) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	return &runtime.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "DemystifyingCRI",
		RuntimeVersion:    "0.1.0",
		RuntimeApiVersion: "v1",
	}, nil
}

// Status is telling Kubelet that everything is alright to avoid node NotReady
func (s *DemystifyingCRI) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	runtimeReady := &runtime.RuntimeCondition{
		Type:   "RuntimeReady",
		Status: true,
	}

	networkReady := &runtime.RuntimeCondition{
		Type:   "NetworkReady",
		Status: true,
	}

	return &runtime.StatusResponse{
		Status: &runtime.RuntimeStatus{
			Conditions: []*runtime.RuntimeCondition{
				runtimeReady,
				networkReady,
			},
		},
	}, nil
}

func (s *DemystifyingCRI) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	var sandboxes []*runtime.PodSandbox
	for _, sandbox := range s.sandboxes {
		sandboxes = append(sandboxes, sandbox)
	}

	return &runtime.ListPodSandboxResponse{Items: sandboxes}, nil
}

func (s *DemystifyingCRI) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	sandboxID := fmt.Sprintf("%s-%s-sandbox", req.Config.Metadata.Namespace, req.Config.Metadata.Name)

	// Check if the sandbox already exists
	if sandbox, exists := s.sandboxes[sandboxID]; exists {
		return &runtime.RunPodSandboxResponse{PodSandboxId: sandbox.Id}, nil
	}

	// Unpack image
	unpackedPath, err := s.unpackImage(s.sandboxImage, sandboxID)
	if err != nil {
		return nil, err
	}

	// Load the existing config.json
	configFilePath := filepath.Join(unpackedPath, "config.json")
	g, err := generate.NewFromFile(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load OCI spec from file: %v", err)
	}

	// Set terminal to false in order to run container detached
	g.Config.Process.Terminal = false

	// Save the updated config.json
	if err := g.SaveToFile(configFilePath, generate.ExportOptions{}); err != nil {
		return nil, fmt.Errorf("failed to save updated OCI spec: %v", err)
	}

	// Use runc to create the PodSandbox
	cmd := exec.Command("runc", "run", "-d", "--bundle", unpackedPath, sandboxID)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to create sandbox with runc: %v", err)
	}

	// Store sandbox info
	s.sandboxes[sandboxID] = &runtime.PodSandbox{
		Id: sandboxID,
		Metadata: &runtime.PodSandboxMetadata{
			Name:      req.Config.Metadata.Name,
			Namespace: req.Config.Metadata.Namespace,
			Uid:       req.Config.Metadata.Uid,
		},
		State:     runtime.PodSandboxState_SANDBOX_READY,
		CreatedAt: time.Now().UnixNano(),
	}

	return &runtime.RunPodSandboxResponse{PodSandboxId: sandboxID}, nil
}

func (s *DemystifyingCRI) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	sandbox, exists := s.sandboxes[req.PodSandboxId]
	if !exists {
		return nil, fmt.Errorf("sandbox %s does not exist", req.PodSandboxId)
	}

	return &runtime.PodSandboxStatusResponse{
		Status: &runtime.PodSandboxStatus{
			Id:        sandbox.Id,
			State:     runtime.PodSandboxState_SANDBOX_READY,
			Metadata:  sandbox.Metadata,
			CreatedAt: sandbox.CreatedAt,
		},
	}, nil
}

func (s *DemystifyingCRI) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	var containers []*runtime.Container
	for _, container := range s.containers {
		containers = append(containers, container)
	}

	return &runtime.ListContainersResponse{Containers: containers}, nil
}

func (s *DemystifyingCRI) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	containerID := fmt.Sprintf("%s-%s", req.PodSandboxId, req.Config.Metadata.Name)

	// Check if the container already exists
	if container, exists := s.containers[containerID]; exists {
		return &runtime.CreateContainerResponse{ContainerId: container.Id}, nil
	}

	// Unpack the image
	unpackedPath, err := s.unpackImage(req.Config.Image.Image, containerID)
	if err != nil {
		return nil, err
	}

	// Get a JSON containing the PID of the sandbox
	stateCmd := exec.Command("runc", "state", req.PodSandboxId)
	stateOut, err := stateCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox state: %v", err)
	}

	// Extract the PID from the JSON
	type RuncState struct {
		Pid int `json:"pid"`
	}
	var state RuncState
	if err := json.Unmarshal(stateOut, &state); err != nil {
		return nil, fmt.Errorf("failed to parse runc state output: %v", err)
	}
	sandboxPid := state.Pid

	// Load the existing config.json
	configFilePath := filepath.Join(unpackedPath, "config.json")
	g, err := generate.NewFromFile(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load OCI spec from file: %v", err)
	}

	// Set terminal to false in order to run container detached
	g.Config.Process.Terminal = false

	// Use sandbox's network namespace
	netNsPath := fmt.Sprintf("/proc/%d/ns/net", sandboxPid)
	if err := g.AddOrReplaceLinuxNamespace("network", netNsPath); err != nil {
		return nil, fmt.Errorf("failed to set network namespace: %v", err)
	}

	// Save the updated config.json
	if err := g.SaveToFile(configFilePath, generate.ExportOptions{}); err != nil {
		return nil, fmt.Errorf("failed to save updated OCI spec: %v", err)
	}

	// Use runc to create the container
	cmd := exec.Command("runc", "run", "-d", "--bundle", unpackedPath, containerID)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to create sandbox with runc: %v", err)
	}

	// Store container info
	s.containers[containerID] = &runtime.Container{
		Id:           containerID,
		PodSandboxId: req.PodSandboxId,
		Metadata:     req.Config.Metadata,
		Image:        req.Config.Image,
		ImageRef:     req.Config.Image.Image,
		State:        runtime.ContainerState_CONTAINER_RUNNING,
		CreatedAt:    time.Now().UnixNano(),
	}

	return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}

// StartContainer must be implemented as Kubelet requires it after CreateContainer was executed
func (s *DemystifyingCRI) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	return &runtime.StartContainerResponse{}, nil
}

func (s *DemystifyingCRI) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	container, exists := s.containers[req.ContainerId]
	if !exists {
		return nil, fmt.Errorf("container %s does not exist", req.ContainerId)
	}

	return &runtime.ContainerStatusResponse{
		Status: &runtime.ContainerStatus{
			Id:        container.Id,
			State:     container.State,
			Metadata:  container.Metadata,
			Image:     container.Image,
			ImageRef:  container.ImageRef,
			CreatedAt: container.CreatedAt,
		},
	}, nil
}

// Implement ImageService methods

func (s *DemystifyingCRI) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	var images []*runtime.Image
	for _, image := range s.images {
		images = append(images, image)
	}

	return &runtime.ListImagesResponse{Images: images}, nil
}

// ImageStatus must be implemented as Kubelet expects a proper response
func (s *DemystifyingCRI) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	imageID := req.Image.Image

	image, exists := s.images[imageID]
	if !exists {
		return &runtime.ImageStatusResponse{
			Image: nil, // This indicates that the image was not found
		}, nil
	}

	return &runtime.ImageStatusResponse{
		Image: &runtime.Image{
			Id:   image.Id,
			Spec: image.Spec,
			Size: image.Size,
		},
	}, nil
}

func (s *DemystifyingCRI) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	err := s.downloadImage(req.Image.Image)
	if err != nil {
		return nil, err
	}

	return &runtime.PullImageResponse{ImageRef: req.Image.Image}, nil
}

func (s *DemystifyingCRI) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	return &runtime.ImageFsInfoResponse{}, nil
}

// downloadImage downloads an image and stores it at imageRoot
func (s *DemystifyingCRI) downloadImage(image string) error {
	_, exists := s.images[image]
	if exists {
		return nil
	}

	// Download image
	dst := filepath.Join(s.imageRoot, getImage(image))
	cmd := exec.Command("skopeo", "copy", "docker://"+image, "oci:"+dst)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to download image %s: %v", image, err)
	}

	// Store image info
	s.images[image] = &runtime.Image{
		Id:   image,
		Spec: &runtime.ImageSpec{Image: image},
		Size: 123456, // Mock size
	}

	return nil
}

// unpackImage unpacks an image and returns the path where it was unpacked
func (s *DemystifyingCRI) unpackImage(image, containerID string) (string, error) {
	snapshotPath := filepath.Join(s.runtimeRoot, containerID)

	// Check if there already is an unpacked image at the location
	_, err := os.Stat(snapshotPath)
	if err == nil {
		return snapshotPath, nil
	}

	imagePath := filepath.Join(s.imageRoot, getImage(image))

	// Unpack image
	cmd := exec.Command("umoci", "unpack", "--image", imagePath, snapshotPath)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to unpack image %s to %s: %v", imagePath, snapshotPath, err)
	}

	return snapshotPath, nil
}

// Start the CRI gRPC server
func main() {
	lis, err := net.Listen("unix", "/var/run/demystifying-cri.sock")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	// Create DemystifyingCRI and initialize maps for storing data about sandboxes, containers, and images
	s := &DemystifyingCRI{
		sandboxes:    make(map[string]*runtime.PodSandbox),
		containers:   make(map[string]*runtime.Container),
		images:       make(map[string]*runtime.Image),
		runtimeRoot:  "/var/lib/demystifying-cri",
		imageRoot:    "/var/lib/demystifying-cri/images",
		sandboxImage: "registry.k8s.io/pause:3.9",
	}

	// Create directory for images
	err = os.MkdirAll(s.imageRoot, 0755)
	if err != nil {
		log.Fatalf("failed to create images directory: %v", err)
	}

	// Download Sandbox image
	err = s.downloadImage(s.sandboxImage)
	if err != nil {
		log.Fatalf("failed to download sandbox image: %v", err)
	}

	grpcServer := grpc.NewServer()

	// Register both RuntimeService and ImageService
	runtime.RegisterRuntimeServiceServer(grpcServer, s)
	runtime.RegisterImageServiceServer(grpcServer, s)

	fmt.Println("CRI server listening on /var/run/demystifying-cri.sock")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
	defer grpcServer.Stop()
}

// getImage takes an image and returns the name of the image without the registry
func getImage(image string) string {
	index := strings.Index(image, "/")
	return image[index+1:]
}
