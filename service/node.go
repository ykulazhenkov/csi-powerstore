/*
 *
 * Copyright © 2020 Dell Inc. or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package service

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/dell/gobrick"
	"github.com/dell/gofsutil"
	"github.com/dell/gopowerstore"
	"google.golang.org/grpc/metadata"
	"math/rand"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"context"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maximumStartupDelay         = 30
	powerStoreMaxNodeNameLength = 64
	blockVolumePathMarker       = "/csi/volumeDevices/publish/"
)

func (s *service) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest) (
	*csi.NodeStageVolumeResponse, error) {
	// Probe the node if required and make sure startup called
	logFields := getLogFields(ctx)
	_, err := s.impl.nodeProbe(ctx)
	if err != nil {
		log.Error("nodeProbe failed with error: " + err.Error())
		return nil, err
	}

	publishContext := req.GetPublishContext()
	var useNFS bool
	fsType, ok := publishContext[keyFsType]
	// FsType can be empty
	if ok {
		useNFS = strings.ToLower(fsType) == "nfs"
	}

	// Get the VolumeID and validate against the volume
	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	var stager VolumeStager

	if useNFS {
		stager = &NFSStager{}
	} else {
		stager = &SCSIStager{}
	}

	return stager.Stage(ctx, req, s, logFields, s.nodeMountLib)
}

func (s *service) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest) (
	*csi.NodeUnstageVolumeResponse, error) {

	logFields := getLogFields(ctx)
	var err error

	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	// append additional path to be able to do bind mounts
	stagingPath := s.nodeMountLib.GetStagingPath(ctx, req)

	logFields["ID"] = volID
	logFields["StagingPath"] = stagingPath
	ctx = setLogFields(ctx, logFields)

	log.WithFields(logFields).Info("calling unstage")

	device, err := s.nodeMountLib.UnstageVolume(ctx, req)
	if err != nil {
		log.WithFields(logFields).Error(err)
		return nil, status.Errorf(codes.Internal, "failed to unstage volume: %s", err.Error())
	}
	if device != "" {
		err := s.volToDevMapper.CreateMapping(volID, device)
		if err != nil {
			log.WithFields(logFields).Warningf("failed to create vol to device mapping: %s", err.Error())
		}
	} else {
		device, err = s.volToDevMapper.GetMapping(volID)
		if err != nil {
			log.WithFields(logFields).Info("no device found. skip device removal")
			return &csi.NodeUnstageVolumeResponse{}, nil
		}
	}
	f := log.Fields{"Device": device}
	log.WithFields(logFields).WithFields(f).Info("unstage complete")
	// TODO: We shouldn't do that for nfs if we can, maybe write info to a file in NodeStage?
	connectorCtx := setLogFields(context.Background(), logFields)
	connectorCtx = copyTraceObj(ctx, connectorCtx)
	if s.useFC {
		err = s.fcConnector.DisconnectVolumeByDeviceName(connectorCtx, device)
	} else {
		err = s.iscsiConnector.DisconnectVolumeByDeviceName(connectorCtx, device)
	}

	if err != nil {
		log.WithFields(logFields).Error(err)
		return nil, err
	}
	log.WithFields(logFields).WithFields(f).Info("block device removal complete")
	err = s.volToDevMapper.DeleteMapping(volID)
	if err != nil {
		log.WithFields(logFields).Warningf("failed to remove vol to Dev mapping: %s", err.Error())
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (s *service) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest) (
	*csi.NodePublishVolumeResponse, error) {

	logFields := getLogFields(ctx)
	// Probe the node if required and make sure startup called
	_, err := s.impl.nodeProbe(ctx)
	if err != nil {
		log.Error("nodeProbe failed with error: " + err.Error())
		return nil, err
	}

	publishContext := req.GetPublishContext()
	var useNFS bool
	fsType, ok := publishContext[keyFsType]
	// FsType can be empty
	if ok {
		useNFS = strings.ToLower(fsType) == "nfs"
	}

	// Get the VolumeID and validate against the volume
	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "targetPath is required")
	}

	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapability is required")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "stagingPath is required")
	}
	// append additional path to be able to do bind mounts
	stagingPath := s.nodeMountLib.GetStagingPath(ctx, req)

	logFields["ID"] = volID
	logFields["TargetPath"] = targetPath
	logFields["StagingPath"] = stagingPath
	logFields["ReadOnly"] = req.GetReadonly()
	ctx = setLogFields(ctx, logFields)

	log.WithFields(logFields).Info("calling publish")
	if useNFS {
		if err := s.nodeMountLib.PublishVolumeNFS(ctx, req); err != nil {
			log.WithFields(logFields).Error(err)
			return nil, err
		}
	} else {
		if err := s.nodeMountLib.PublishVolume(ctx, req); err != nil {
			log.WithFields(logFields).Error(err)
			return nil, err
		}
	}
	log.WithFields(logFields).Info("publish complete")
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *service) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest) (
	*csi.NodeUnpublishVolumeResponse, error) {

	logFields := getLogFields(ctx)
	var err error

	target := req.GetTargetPath()
	if target == "" {
		log.Error("target path required")
		return nil, status.Error(codes.InvalidArgument, "target path required")
	}

	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	logFields["ID"] = volID
	logFields["TargetPath"] = target
	ctx = setLogFields(ctx, logFields)

	log.WithFields(logFields).Info("calling unpublish")

	if err = s.nodeMountLib.UnpublishVolume(ctx, req); err != nil {
		log.WithFields(logFields).Error(err)
		return nil, err
	}
	log.WithFields(logFields).Info("unpublish complete")
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (s *service) NodeGetCapabilities(
	ctx context.Context,
	req *csi.NodeGetCapabilitiesRequest) (
	*csi.NodeGetCapabilitiesResponse, error) {

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
				},
			},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (s *service) NodeGetInfo(
	ctx context.Context,
	req *csi.NodeGetInfoRequest) (
	*csi.NodeGetInfoResponse, error) {

	if len(s.nodeID) <= 0 {
		if err := s.impl.updateNodeID(); err != nil {
			return nil, err
		}
	}

	return &csi.NodeGetInfoResponse{
		NodeId: s.nodeID,
	}, nil
}

func (s *service) NodeGetVolumeStats(
	ctx context.Context,
	req *csi.NodeGetVolumeStatsRequest) (
	*csi.NodeGetVolumeStatsResponse, error) {

	return nil, status.Error(codes.Unimplemented, "")
}

func (si *serviceIMPL) nodeProbe(ctx context.Context) (bool, error) {
	if err := si.implProxy.initPowerStoreClient(); err != nil {
		return false, err
	}
	if err := si.implProxy.updateNodeID(); err != nil {
		return false, err
	}
	si.implProxy.initNodeFSLib()
	si.implProxy.initNodeMountLib()
	si.implProxy.initNodeVolToDevMapper()
	si.implProxy.initISCSIConnector()
	si.implProxy.initFCConnector()
	return si.service.nodeIsInitialized, nil
}

func (si *serviceIMPL) updateNodeID() error {
	if si.service.nodeID == "" {
		hostID, err := si.service.fileReader.ReadFile(si.service.opts.NodeIDFilePath)
		if err != nil {
			log.WithFields(log.Fields{
				"path":  si.service.opts.NodeIDFilePath,
				"error": err,
			}).Error("Could not read Node ID file")
			return status.Errorf(codes.FailedPrecondition,
				"Could not readNode ID file: %s", err.Error())
		}
		ip := getOutboundIP()
		nodeID := fmt.Sprintf(
			"%s-%s-%s", si.service.opts.NodeNamePrefix, strings.TrimSpace(string(hostID)), ip.String())
		if len(nodeID) > powerStoreMaxNodeNameLength {
			err := errors.New("node name prefix is too long")
			log.WithFields(log.Fields{
				"value": si.service.opts.NodeNamePrefix,
				"error": err,
			}).Error("Invalid Node ID")
			return err
		}
		si.service.nodeID = nodeID
	}
	return nil
}

// Get preferred outbound ip of this machine
func getOutboundIP() net.IP {
	conn, err := net.Dial("udp", "192.168.100.1:80")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}

func (si *serviceIMPL) initNodeFSLib() {
	if si.service.nodeFSLib == nil {
		si.service.nodeFSLib = &gofsutilWrapper{}
	}
}

func (si *serviceIMPL) initNodeMountLib() {
	if si.service.nodeMountLib == nil {
		si.service.nodeMountLib = &mount{}
	}
}

func (si *serviceIMPL) initISCSIConnector() {
	if si.service.iscsiConnector == nil {
		setupGobrick(si.service)
		si.service.iscsiConnector = gobrick.NewISCSIConnector(
			gobrick.ISCSIConnectorParams{Chroot: si.service.opts.NodeChrootPath})
	}
}

func (si *serviceIMPL) initFCConnector() {
	if si.service.fcConnector == nil {
		setupGobrick(si.service)
		si.service.fcConnector = gobrick.NewFCConnector(
			gobrick.FCConnectorParams{Chroot: si.service.opts.NodeChrootPath})
	}
}

func setupGobrick(srv *service) {
	gobrick.SetLogger(&customLogger{})
	if srv.opts.EnableTracing {
		gobrick.SetTracer(&customTracer{})
	}
}

func (si *serviceIMPL) initNodeVolToDevMapper() {
	if si.service.volToDevMapper == nil {
		si.service.volToDevMapper = &volToDevFile{
			W:       si.service.fileWriter,
			R:       si.service.fileReader,
			OS:      si.service.os,
			DataDir: si.service.opts.TmpDir,
		}
	}
}

func (si *serviceIMPL) readISCSITargetsFromPublishContext(pc map[string]string) []ISCSITargetInfo {
	var targets []ISCSITargetInfo
	for i := 0; ; i++ {
		target := ISCSITargetInfo{}
		t, tfound := pc[fmt.Sprintf("%s%d", PublishContextISCSITargetsPrefix, i)]
		if tfound {
			target.Target = t
		}
		p, pfound := pc[fmt.Sprintf("%s%d", PublishContextISCSIPortalsPrefix, i)]
		if pfound {
			target.Portal = p
		}
		if !tfound || !pfound {
			break
		}
		targets = append(targets, target)
	}
	log.Infof("iSCSI iscsiTargets from context: %v", targets)
	return targets
}

func (si *serviceIMPL) readFCTargetsFromPublishContext(pc map[string]string) []FCTargetInfo {
	var targets []FCTargetInfo
	for i := 0; ; i++ {
		wwpn, tfound := pc[fmt.Sprintf("%s%d", PublishContextFCWWPNPrefix, i)]
		if !tfound {
			break
		}
		targets = append(targets, FCTargetInfo{WWPN: wwpn})
	}
	log.Infof("FC iscsiTargets from context: %v", targets)
	return targets
}

// nodeStartup performs a few necessary functions for the nodes to function properly
// - validates that at least one iSCSI initiator is defined
// - validates that a connection to PowerStore exists
// - invokes nodeHostSetup in a thread
//
// returns halt service if unable to register node
func (si *serviceIMPL) nodeStartup(ctx context.Context, gs gracefulStopper) error {

	if si.service.nodeIsInitialized {
		return nil
	}

	// make sure we have a connection to PowerStore
	if si.service.adminClient == nil {
		return fmt.Errorf("there is no PowerStore connection")
	}

	var err error
	var initiators []string
	if si.service.opts.PreferredTransport != noneTransport {
		var iscsiAvailable bool
		var fcAvailable bool

		iscsiInitiators, err := si.service.iscsiConnector.GetInitiatorName(ctx)
		if err != nil {
			log.Error("nodeStartup could not GetInitiatorIQNs")
		} else if len(iscsiInitiators) == 0 {
			log.Error("iscsi initiators not found on node")
		} else {
			log.Error("iscsi initiators found on node")
			iscsiAvailable = true
		}

		fcInitiators, err := si.implProxy.getNodeFCPorts(ctx)
		if err != nil {
			log.Error("nodeStartup could not FC initiators for node")
		} else if len(fcInitiators) == 0 {
			log.Error("FC was not found or filtered with FCPortsFilterFile")
		} else {
			log.Error("FC initiators found on node")
			fcAvailable = true
		}

		if !iscsiAvailable && !fcAvailable {
			return fmt.Errorf("FC and iSCSI initiators not found on node")
		}

		switch si.service.opts.PreferredTransport {
		case iSCSITransport:
			if !iscsiAvailable {
				return fmt.Errorf("iSCSI transport was requested but iSCSI initiator is not available")
			}
			si.service.useFC = false
		case fcTransport:
			if !fcAvailable {
				return fmt.Errorf("FC transport was requested but FC initiator is not available")
			}
			si.service.useFC = true
		default:
			si.service.useFC = fcAvailable
		}

		if si.service.useFC {
			initiators = fcInitiators
		} else {
			initiators = iscsiInitiators
		}
	}

	go func() {
		err := si.implProxy.nodeHostSetup(initiators, si.service.useFC, maximumStartupDelay)
		if err != nil {
			log.Errorf("error during nodeHostSetup: %s", err.Error())
			gs.GracefulStop(ctx)
		}
	}()

	return err
}

// nodeHostSetup performs a node registration on storage array
func (si *serviceIMPL) nodeHostSetup(initiators []string, useFC bool, maximumStartupDelay int) error {
	si.service.nodeRescanMutex.Lock()
	defer si.service.nodeRescanMutex.Unlock()
	log.Info("**************************\nnodeHostSetup executing...\n*******************************")
	defer log.Info("**************************\nnodeHostSetup completed...\n*******************************")

	// we need to randomize a time before starting the interaction with PowerStore
	// in order to reduce the concurrent workload on the system

	// determine a random delay period (in
	rand.Seed(time.Now().UTC().UnixNano())
	period := rand.Int() % maximumStartupDelay // #nosec G404
	// sleep ...
	log.Printf("Waiting for %d seconds", period)
	time.Sleep(time.Duration(period) * time.Second)

	if si.service.nodeID == "" {
		return fmt.Errorf("nodeID not set")
	}

	// register node on PowerStore
	if len(initiators) > 0 {
		err := si.implProxy.createOrUpdateHost(context.Background(), useFC, initiators)
		if err != nil {
			log.Error(err.Error())
			return err
		}
	}

	si.service.nodeIsInitialized = true

	return nil
}

// create or update host on PowerStore array
func (si *serviceIMPL) createOrUpdateHost(ctx context.Context, useFC bool, initiators []string) (err error) {
	host, err := si.service.adminClient.GetHostByName(ctx, si.service.nodeID)
	if err != nil {
		apiError, ok := err.(gopowerstore.APIError)
		if ok && apiError.HostIsNotExist() {
			_, err = si.implProxy.createHost(ctx, useFC, initiators)
			return err
		}
		return err
	}
	initiatorsToAdd, initiatorsToDelete := checkIQNS(initiators, host)
	return si.implProxy.modifyHostInitiators(ctx, host.ID, useFC, initiatorsToAdd, initiatorsToDelete)
}

// register host
func (si *serviceIMPL) createHost(ctx context.Context, useFC bool, initiators []string) (id string, err error) {
	osType := gopowerstore.OSTypeEnumLinux
	reqInitiators := buildInitiatorsArray(useFC, initiators)
	description := fmt.Sprintf("k8s node: %s", si.service.opts.KubeNodeName)
	createParams := gopowerstore.HostCreate{Name: &si.service.nodeID, OsType: &osType, Initiators: &reqInitiators,
		Description: &description}
	resp, err := si.service.adminClient.CreateHost(ctx, &createParams)
	if err != nil {
		return id, err
	}
	return resp.ID, err
}

// add or remove initiators from host
func (si *serviceIMPL) modifyHostInitiators(ctx context.Context, hostID string, useFC bool,
	initiatorsToAdd []string, initiatorsToDelete []string) error {
	if len(initiatorsToDelete) > 0 {
		modifyParams := gopowerstore.HostModify{}
		modifyParams.RemoveInitiators = &initiatorsToDelete
		_, err := si.service.adminClient.ModifyHost(ctx, &modifyParams, hostID)
		if err != nil {
			return err
		}
	}
	if len(initiatorsToAdd) > 0 {
		modifyParams := gopowerstore.HostModify{}
		initiators := buildInitiatorsArray(useFC, initiatorsToAdd)
		modifyParams.AddInitiators = &initiators
		_, err := si.service.adminClient.ModifyHost(ctx, &modifyParams, hostID)
		if err != nil {
			return err
		}
	}
	return nil
}

func checkIQNS(IQNs []string, host gopowerstore.Host) (iqnToAdd, iqnToDelete []string) {
	// create map with initiators which are already exist
	initiatorMap := make(map[string]bool)
	for _, initiator := range host.Initiators {
		initiatorMap[initiator.PortName] = false
	}

	for _, iqn := range IQNs {
		_, ok := initiatorMap[iqn]
		if ok {
			// the iqn should be left in the host
			initiatorMap[iqn] = true
		} else {
			// the iqn should be added to the host
			iqnToAdd = append(iqnToAdd, iqn)
		}
	}

	// find iqns to delete from host
	for iqn, found := range initiatorMap {
		if !found {
			iqnToDelete = append(iqnToDelete, iqn)
		}
	}
	return
}

func buildInitiatorsArray(useFC bool, initiators []string) []gopowerstore.InitiatorCreateModify {
	var portType gopowerstore.InitiatorProtocolTypeEnum
	if useFC {
		portType = gopowerstore.InitiatorProtocolTypeEnumFC
	} else {
		portType = gopowerstore.InitiatorProtocolTypeEnumISCSI
	}
	initiatorsReq := make([]gopowerstore.InitiatorCreateModify, len(initiators))
	for i, iqn := range initiators {
		iqn := iqn
		initiatorsReq[i] = gopowerstore.InitiatorCreateModify{PortName: &iqn, PortType: &portType}
	}
	return initiatorsReq
}

func (si *serviceIMPL) readFCPortsFilterFile(ctx context.Context) ([]string, error) {
	if si.service.opts.FCPortsFilterFilePath == "" {
		return nil, nil
	}
	data, err := si.service.fileReader.ReadFile(si.service.opts.FCPortsFilterFilePath)
	if err != nil {
		if si.service.os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var result []string
	wwpns := strings.Split(strings.TrimSpace(string(data)), ",")
	for _, p := range wwpns {
		if !strings.Contains(p, ":") {
			log.Error("invalid FCPortsFilterFile format")
			return nil, nil
		}
		result = append(result, p)
	}
	return result, nil
}

func (si *serviceIMPL) getNodeFCPorts(ctx context.Context) ([]string, error) {
	var err error
	var initiators []string

	defer func() {
		initiators := initiators
		log.Infof("FC initiators found: %s", initiators)
	}()

	rawInitiatorsData, err := si.service.fcConnector.GetInitiatorPorts(ctx)
	if err != nil {
		log.Error("failed FC initiators list from node")
		return nil, err
	}

	for _, initiator := range rawInitiatorsData {
		data, err := formatWWPN(strings.TrimPrefix(initiator, "0x"))
		if err != nil {
			return nil, err
		}
		initiators = append(initiators, data)
	}
	if len(initiators) == 0 {
		return initiators, nil
	}
	portsFilter, _ := si.implProxy.readFCPortsFilterFile(ctx)
	if len(portsFilter) == 0 {
		return initiators, nil
	}
	var filteredInitiators []string
	for _, filterValue := range portsFilter {
		for _, initiator := range initiators {
			if initiator != filterValue {
				continue
			}
			log.Infof("FC initiator port %s match filter", initiator)
			filteredInitiators = append(filteredInitiators, initiator)
		}
	}
	initiators = filteredInitiators

	return initiators, nil

}

func formatWWPN(data string) (string, error) {
	var buffer bytes.Buffer
	for i, v := range data {
		_, err := buffer.WriteRune(v)
		if err != nil {
			return "", err
		}
		if i%2 != 0 && i < len(data)-1 {
			_, err := buffer.WriteString(":")
			if err != nil {
				return "", err
			}
		}
	}
	return buffer.String(), nil
}

//NodeExpandVolume helps extending a volume size on a node
func (s *service) NodeExpandVolume(
	ctx context.Context,
	req *csi.NodeExpandVolumeRequest) (
	*csi.NodeExpandVolumeResponse, error) {

	var reqID string
	var err error
	headers, ok := metadata.FromIncomingContext(ctx)
	if ok {
		if req, ok := headers["csi.requestid"]; ok && len(req) > 0 {
			reqID = req[0]
		}
	}

	// Probe the node if required and make sure startup called
	_, err = s.impl.nodeProbe(ctx)
	if err != nil {
		log.Error("nodeProbe failed with error: " + err.Error())
		return nil, err
	}

	// Get the VolumeID and validate against the volume
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	targetPath := req.GetVolumePath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "targetPath is required")
	}
	isBlock := strings.Contains(targetPath, blockVolumePathMarker)

	// Parse the CSI VolumeId and validate against the volume
	vol, err := s.adminClient.GetVolume(ctx, id)
	if err != nil {
		// If the volume isn't found, we cannot stage it
		return nil, status.Error(codes.NotFound, "Volume not found")
	}
	volumeWWN := vol.Wwn

	//Locate and fetch all (multipath/regular) mounted paths using this volume
	var devMnt *gofsutil.DeviceMountInfo
	var targetmount string
	devMnt, err = gofsutil.GetMountInfoFromDevice(ctx, vol.Name)
	if err != nil {
		if isBlock {
			log.WithField("CSIRequestID", reqID).Info("Block device detected- node expansion not required")
			return &csi.NodeExpandVolumeResponse{}, nil
		}
		log.Infof("Failed to find mount info for (%s) with error (%s)", vol.Name, err.Error())
		log.Info("Probably offline volume expansion. Will try to perform a temporary mount.")
		var disklocation string

		disklocation = fmt.Sprintf("%s/%s", targetPath, vol.ID)
		log.Infof("DisklLocation: %s", disklocation)
		targetmount = fmt.Sprintf("tmp/%s/%s", vol.ID, vol.Name)
		log.Infof("TargetMount: %s", targetmount)
		err = os.MkdirAll(targetmount, 0750)
		if err != nil {
			return nil, status.Error(codes.Internal,
				fmt.Sprintf("Failed to find mount info for (%s) with error (%s)", vol.Name, err.Error()))
		}
		err = gofsutil.Mount(ctx, disklocation, targetmount, "")
		if err != nil {
			return nil, status.Error(codes.Internal,
				fmt.Sprintf("Failed to find mount info for (%s) with error (%s)", vol.Name, err.Error()))
		}

		defer func() {
			if targetmount != "" {
				log.Infof("Clearing down temporary mount points in: %s", targetmount)
				err := gofsutil.Unmount(ctx, targetmount)
				if err != nil {
					log.Error("Failed to remove temporary mount points")
				}
				err = os.RemoveAll(targetmount)
				if err != nil {
					log.Error("Failed to remove temporary mount points")
				}
			}
		}()

		devMnt, err = gofsutil.GetMountInfoFromDevice(ctx, vol.Name)
		if err != nil {
			return nil, status.Error(codes.Internal,
				fmt.Sprintf("Failed to find mount info for (%s) with error (%s)", vol.Name, err.Error()))
		}

	}

	log.Infof("Mount info for volume %s: %+v", vol.Name, devMnt)

	size := req.GetCapacityRange().GetRequiredBytes()

	f := log.Fields{
		"CSIRequestID": reqID,
		"VolumeName":   vol.Name,
		"VolumePath":   targetPath,
		"Size":         size,
		"VolumeWWN":    volumeWWN,
	}
	log.WithFields(f).Info("Calling resize the file system")

	// Rescan the device for the volume expanded on the array
	for _, device := range devMnt.DeviceNames {
		devicePath := "/sys/block/" + device
		err = gofsutil.DeviceRescan(context.Background(), devicePath)
		if err != nil {
			log.Errorf("Failed to rescan device (%s) with error (%s)", devicePath, err.Error())
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	// Expand the filesystem with the actual expanded volume size.
	if devMnt.MPathName != "" {
		err = gofsutil.ResizeMultipath(context.Background(), devMnt.MPathName)
		if err != nil {
			log.Errorf("Failed to resize filesystem: device  (%s) with error (%s)", devMnt.MountPoint, err.Error())

			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	//For a regular device, get the device path (devMnt.DeviceNames[1]) where the filesystem is mounted
	//PublishVolume creates devMnt.DeviceNames[0] but is left unused for regular devices
	var devicePath string
	if len(devMnt.DeviceNames) > 1 {
		devicePath = "/dev/" + devMnt.DeviceNames[1]
	} else {
		devicePath = "/dev/" + devMnt.DeviceNames[0]
	}
	fsType, err := gofsutil.FindFSType(context.Background(), devMnt.MountPoint)
	if err != nil {
		log.Errorf("Failed to fetch filesystem for volume  (%s) with error (%s)", devMnt.MountPoint, err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}
	log.Infof("Found %s filesystem mounted on volume %s", fsType, devMnt.MountPoint)
	//Resize the filesystem
	err = gofsutil.ResizeFS(context.Background(), devMnt.MountPoint, devicePath, devMnt.MPathName, fsType)
	if err != nil {
		log.Errorf("Failed to resize filesystem: mountpoint (%s) device (%s) with error (%s)",
			devMnt.MountPoint, devicePath, err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

type volToDevFile struct {
	W       fileWriter
	R       fileReader
	OS      limitedOSIFace
	DataDir string
}

func (vtd *volToDevFile) CreateMapping(volID, deviceName string) error {
	return vtd.W.WriteFile(path.Join(vtd.DataDir, volID), []byte(deviceName), 0640)
}

func (vtd *volToDevFile) GetMapping(volID string) (string, error) {
	data, err := vtd.R.ReadFile(path.Join(vtd.DataDir, volID))
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", errors.New("no device name in mapping")
	}
	return string(data), nil
}

func (vtd *volToDevFile) DeleteMapping(volID string) error {
	err := vtd.OS.Remove(path.Join(vtd.DataDir, volID))
	if vtd.OS.IsNotExist(err) {
		return nil
	}
	return err
}
