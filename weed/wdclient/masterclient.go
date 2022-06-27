package wdclient

import (
	"context"
	"github.com/chrislusf/seaweedfs/weed/stats"
	"math/rand"
	"time"

	"github.com/chrislusf/seaweedfs/weed/util"
	"google.golang.org/grpc"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb"
	"github.com/chrislusf/seaweedfs/weed/pb/master_pb"
)

type MasterClient struct {
	FilerGroup     string
	clientType     string
	clientHost     pb.ServerAddress
	currentMaster  pb.ServerAddress
	masters        map[string]pb.ServerAddress
	grpcDialOption grpc.DialOption

	vidMap

	OnPeerUpdate func(update *master_pb.ClusterNodeUpdate, startFrom time.Time)
}

func NewMasterClient(grpcDialOption grpc.DialOption, filerGroup string, clientType string, clientHost pb.ServerAddress, clientDataCenter string, masters map[string]pb.ServerAddress) *MasterClient {
	return &MasterClient{
		FilerGroup:     filerGroup,
		clientType:     clientType,
		clientHost:     clientHost,
		masters:        masters,
		grpcDialOption: grpcDialOption,
		vidMap:         newVidMap(clientDataCenter),
	}
}

func (mc *MasterClient) GetLookupFileIdFunction() LookupFileIdFunctionType {
	return mc.LookupFileIdWithFallback
}

func (mc *MasterClient) LookupFileIdWithFallback(fileId string) (fullUrls []string, err error) {
	fullUrls, err = mc.vidMap.LookupFileId(fileId)
	if err == nil {
		return
	}
	err = pb.WithMasterClient(false, mc.currentMaster, mc.grpcDialOption, func(client master_pb.SeaweedClient) error {
		resp, err := client.LookupVolume(context.Background(), &master_pb.LookupVolumeRequest{
			VolumeOrFileIds: []string{fileId},
		})
		if err != nil {
			return err
		}
		for vid, vidLocation := range resp.VolumeIdLocations {
			for _, vidLoc := range vidLocation.Locations {
				loc := Location{
					Url:       vidLoc.Url,
					PublicUrl: vidLoc.PublicUrl,
					GrpcPort:  int(vidLoc.GrpcPort),
				}
				mc.vidMap.addLocation(uint32(vid), loc)
				fullUrls = append(fullUrls, "http://"+loc.Url+"/"+fileId)
			}
		}

		return nil
	})
	return
}

func (mc *MasterClient) GetMaster() pb.ServerAddress {
	mc.WaitUntilConnected()
	return mc.currentMaster
}

func (mc *MasterClient) GetMasters() map[string]pb.ServerAddress {
	mc.WaitUntilConnected()
	return mc.masters
}

func (mc *MasterClient) WaitUntilConnected() {
	for mc.currentMaster == "" {
		time.Sleep(time.Duration(rand.Int31n(200)) * time.Millisecond)
	}
}

func (mc *MasterClient) KeepConnectedToMaster() {
	glog.V(1).Infof("%s.%s masterClient bootstraps with masters %v", mc.FilerGroup, mc.clientType, mc.masters)
	for {
		mc.tryAllMasters()
		time.Sleep(time.Second)
	}
}

func (mc *MasterClient) FindLeaderFromOtherPeers(myMasterAddress pb.ServerAddress) (leader string) {
	for _, master := range mc.masters {
		if master == myMasterAddress {
			continue
		}
		if grpcErr := pb.WithMasterClient(false, master, mc.grpcDialOption, func(client master_pb.SeaweedClient) error {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			defer cancel()
			resp, err := client.GetMasterConfiguration(ctx, &master_pb.GetMasterConfigurationRequest{})
			if err != nil {
				return err
			}
			leader = resp.Leader
			return nil
		}); grpcErr != nil {
			glog.V(0).Infof("connect to %s: %v", master, grpcErr)
		}
		if leader != "" {
			glog.V(0).Infof("existing leader is %s", leader)
			return
		}
	}
	glog.V(0).Infof("No existing leader found!")
	return
}

func (mc *MasterClient) tryAllMasters() {
	var nextHintedLeader pb.ServerAddress
	for _, master := range mc.masters {

		nextHintedLeader = mc.tryConnectToMaster(master)
		for nextHintedLeader != "" {
			nextHintedLeader = mc.tryConnectToMaster(nextHintedLeader)
		}

		mc.currentMaster = ""
	}
}

func (mc *MasterClient) tryConnectToMaster(master pb.ServerAddress) (nextHintedLeader pb.ServerAddress) {
	glog.V(1).Infof("%s.%s masterClient Connecting to master %v", mc.FilerGroup, mc.clientType, master)
	stats.MasterClientConnectCounter.WithLabelValues("total").Inc()
	gprcErr := pb.WithMasterClient(true, master, mc.grpcDialOption, func(client master_pb.SeaweedClient) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		stream, err := client.KeepConnected(ctx)
		if err != nil {
			glog.V(1).Infof("%s.%s masterClient failed to keep connected to %s: %v", mc.FilerGroup, mc.clientType, master, err)
			stats.MasterClientConnectCounter.WithLabelValues(stats.FailedToKeepConnected).Inc()
			return err
		}

		if err = stream.Send(&master_pb.KeepConnectedRequest{
			FilerGroup:    mc.FilerGroup,
			ClientType:    mc.clientType,
			ClientAddress: string(mc.clientHost),
			Version:       util.Version(),
		}); err != nil {
			glog.V(0).Infof("%s.%s masterClient failed to send to %s: %v", mc.FilerGroup, mc.clientType, master, err)
			stats.MasterClientConnectCounter.WithLabelValues(stats.FailedToSend).Inc()
			return err
		}
		glog.V(1).Infof("%s.%s masterClient Connected to %v", mc.FilerGroup, mc.clientType, master)

		resp, err := stream.Recv()
		if err != nil {
			glog.V(0).Infof("%s.%s masterClient failed to receive from %s: %v", mc.FilerGroup, mc.clientType, master, err)
			stats.MasterClientConnectCounter.WithLabelValues(stats.FailedToReceive).Inc()
			return err
		}

		// check if it is the leader to determine whether to reset the vidMap
		if resp.VolumeLocation != nil && resp.VolumeLocation.Leader != "" && string(master) != resp.VolumeLocation.Leader {
			glog.V(0).Infof("master %v redirected to leader %v", master, resp.VolumeLocation.Leader)
			nextHintedLeader = pb.ServerAddress(resp.VolumeLocation.Leader)
			stats.MasterClientConnectCounter.WithLabelValues(stats.RedirectedToleader).Inc()
			return nil
		}

		mc.currentMaster = master
		mc.vidMap = newVidMap("")

		for {
			resp, err := stream.Recv()
			if err != nil {
				glog.V(0).Infof("%s.%s masterClient failed to receive from %s: %v", mc.FilerGroup, mc.clientType, master, err)
				stats.MasterClientConnectCounter.WithLabelValues(stats.FailedToReceive).Inc()
				return err
			}

			if resp.VolumeLocation != nil {
				// maybe the leader is changed
				if resp.VolumeLocation.Leader != "" && string(mc.currentMaster) != resp.VolumeLocation.Leader {
					glog.V(0).Infof("currentMaster %v redirected to leader %v", mc.currentMaster, resp.VolumeLocation.Leader)
					nextHintedLeader = pb.ServerAddress(resp.VolumeLocation.Leader)
					stats.MasterClientConnectCounter.WithLabelValues(stats.RedirectedToleader).Inc()
					return nil
				}

				// process new volume location
				loc := Location{
					Url:        resp.VolumeLocation.Url,
					PublicUrl:  resp.VolumeLocation.PublicUrl,
					DataCenter: resp.VolumeLocation.DataCenter,
					GrpcPort:   int(resp.VolumeLocation.GrpcPort),
				}
				for _, newVid := range resp.VolumeLocation.NewVids {
					glog.V(1).Infof("%s.%s: %s masterClient adds volume %d", mc.FilerGroup, mc.clientType, loc.Url, newVid)
					mc.addLocation(newVid, loc)
				}
				for _, deletedVid := range resp.VolumeLocation.DeletedVids {
					glog.V(1).Infof("%s.%s: %s masterClient removes volume %d", mc.FilerGroup, mc.clientType, loc.Url, deletedVid)
					mc.deleteLocation(deletedVid, loc)
				}
				for _, newEcVid := range resp.VolumeLocation.NewEcVids {
					glog.V(1).Infof("%s.%s: %s masterClient adds ec volume %d", mc.FilerGroup, mc.clientType, loc.Url, newEcVid)
					mc.addEcLocation(newEcVid, loc)
				}
				for _, deletedEcVid := range resp.VolumeLocation.DeletedEcVids {
					glog.V(1).Infof("%s.%s: %s masterClient removes ec volume %d", mc.FilerGroup, mc.clientType, loc.Url, deletedEcVid)
					mc.deleteEcLocation(deletedEcVid, loc)
				}
			}

			if resp.ClusterNodeUpdate != nil {
				update := resp.ClusterNodeUpdate
				if mc.OnPeerUpdate != nil {
					if update.FilerGroup == mc.FilerGroup {
						if update.IsAdd {
							glog.V(0).Infof("+ %s.%s %s leader:%v\n", update.FilerGroup, update.NodeType, update.Address, update.IsLeader)
						} else {
							glog.V(0).Infof("- %s.%s %s leader:%v\n", update.FilerGroup, update.NodeType, update.Address, update.IsLeader)
						}
						stats.MasterClientConnectCounter.WithLabelValues(stats.OnPeerUpdate).Inc()
						mc.OnPeerUpdate(update, time.Now())
					}
				}
			}

		}

	})
	if gprcErr != nil {
		stats.MasterClientConnectCounter.WithLabelValues(stats.Failed).Inc()
		glog.V(1).Infof("%s.%s masterClient failed to connect with master %v: %v", mc.FilerGroup, mc.clientType, master, gprcErr)
	}
	return
}

func (mc *MasterClient) WithClient(streamingMode bool, fn func(client master_pb.SeaweedClient) error) error {
	return util.Retry("master grpc", func() error {
		for mc.currentMaster == "" {
			time.Sleep(3 * time.Second)
		}
		return pb.WithMasterClient(streamingMode, mc.currentMaster, mc.grpcDialOption, func(client master_pb.SeaweedClient) error {
			return fn(client)
		})
	})
}
