package alibaba

import (
	"context"
	"fmt"
	"strings"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/pvtz"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	util_errors "k8s.io/apimachinery/pkg/util/errors"
	ctx2 "k8s.io/cloud-provider-alibaba-cloud/pkg/context"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/provider"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/provider/metadata"
)

const (
	DescribeZoneRecordPageSize = 50
	// TODO add remark
	ZoneRecordRemark = "record.managed.by.ack.ccm"
)

type PVTZProvider struct {
	client *pvtz.Client
	zoneId string
}

func NewPVTZProvider(auth *metadata.ClientAuth) *PVTZProvider {
	return &PVTZProvider{
		client: auth.PVTZ,
		zoneId: ctx2.CFG.Global.PrivateZoneID,
	}
}
func (p *PVTZProvider) ListPVTZ(ctx context.Context) ([]*provider.PvtzEndpoint, error) {
	return p.SearchPVTZ(ctx, &provider.PvtzEndpoint{}, false)
}

func (p *PVTZProvider) SearchPVTZ(ctx context.Context, ep *provider.PvtzEndpoint, exact bool) ([]*provider.PvtzEndpoint, error) {
	req := pvtz.CreateDescribeZoneRecordsRequest()
	req.ZoneId = p.zoneId
	req.PageSize = requests.NewInteger(DescribeZoneRecordPageSize)
	if ep.Rr != "" {
		req.Keyword = ep.Rr
		if exact {
			req.SearchMode = "EXACT"
		} else {
			req.SearchMode = "LIKE"
		}
	}
	records := make([]pvtz.Record, 0)
	pageNumber := 1
	for {
		req.PageNumber = requests.NewInteger(pageNumber)
		resp, err := p.client.DescribeZoneRecords(req)
		if err != nil {
			return nil, err
		}
		for _, record := range resp.Records.Record {
			if p.filterUnsupportedDNSRecordTypes(record) {
				continue
			}
			if p.filterUnmanagedDNSRecord(record) {
				continue
			}
			records = append(records, record)
		}
		if pageNumber < resp.TotalPages {
			pageNumber++
		} else {
			break
		}
	}
	// transform raw zone records into endpoints
	typedEndpointsMap := make(map[string]map[string]*provider.PvtzEndpoint)
	for _, record := range records {
		if endpointsMap := typedEndpointsMap[record.Type]; endpointsMap == nil {
			typedEndpointsMap[record.Type] = make(map[string]*provider.PvtzEndpoint)
		}

		if rrMap := typedEndpointsMap[record.Type][record.Rr]; rrMap == nil {
			typedEndpointsMap[record.Type][record.Rr] = &provider.PvtzEndpoint{
				Rr: record.Rr,
				Values: []provider.PvtzValue{{
					Data:     record.Value,
					RecordId: record.RecordId,
				}},
				Ttl:  int64(record.Ttl),
				Type: record.Type,
			}
		} else {
			typedEndpointsMap[record.Type][record.Rr].Values = append(typedEndpointsMap[record.Type][record.Rr].Values, provider.PvtzValue{
				Data:     record.Value,
				RecordId: record.RecordId,
			})
		}
	}
	totalEndpoints := make([]*provider.PvtzEndpoint, 0)
	for _, endpointsMap := range typedEndpointsMap {
		for _, endpoint := range endpointsMap {
			totalEndpoints = append(totalEndpoints, endpoint)
		}
	}
	return totalEndpoints, nil
}

func (p *PVTZProvider) UpdatePVTZ(ctx context.Context, ep *provider.PvtzEndpoint) error {
	rlog := log.WithFields(log.Fields{"endpointRr": ep.Rr, "endpointType": ep.Type})
	newValues := ep.Values
	oldValues := make([]provider.PvtzValue, 0)
	err := p.record(context.TODO(), ep)
	if err != nil {
		return errors.Wrap(err, "UpdatePVTZ query old zone records error")
	} else {
		oldValues = ep.Values
	}
	rlog.Infof("old endpoints %v, new endpoints %v", oldValues, newValues)
	valueToAdd := make([]string, 0)
	for _, newVal := range newValues {
		if !newVal.InVals(oldValues) {
			valueToAdd = append(valueToAdd, newVal.Data)
		}
	}
	recordIdToDelete := make([]int64, 0)
	for _, oldVal := range oldValues {
		if !oldVal.InVals(newValues) {
			recordIdToDelete = append(recordIdToDelete, oldVal.RecordId)
		}
	}
	errs := make([]error, 0)
	var resp *pvtz.AddZoneRecordResponse
	for _, val := range valueToAdd {
		resp, err = p.create(ep.Type, ep.Rr, val, int(ep.Ttl))
		if err != nil {
			errs = append(errs, err)
		}
		// TODO should have set remark when creating zone record
		err = p.remark(resp.RecordId)
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, id := range recordIdToDelete {
		err = p.delete(id)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Wrap(util_errors.NewAggregate(errs), "UpdatePVTZ update zone records error")
}

func (p *PVTZProvider) DeletePVTZ(ctx context.Context, ep *provider.PvtzEndpoint) error {
	err := p.record(context.TODO(), ep)
	if err != nil {
		return errors.Wrap(err, "DeletePVTZ query old zone records error")
	}
	oldValues := ep.Values
	errs := make([]error, 0)
	for _, val := range oldValues {
		err = p.delete(val.RecordId)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Wrap(util_errors.NewAggregate(errs), "DeletePVTZ deleting old endpoint error")
}

func (p *PVTZProvider) filterUnmanagedDNSRecord(record pvtz.Record) bool {
	if strings.Contains(record.Remark, ZoneRecordRemark) {
		return false
	}
	return true
}

func (p *PVTZProvider) filterUnsupportedDNSRecordTypes(record pvtz.Record) bool {
	switch record.Type {
	case provider.RecordTypeA, provider.RecordTypeCNAME, provider.RecordTypePTR, provider.RecordTypeSRV, provider.RecordTypeTXT:
		return false
	default:
		return true
	}
}

func (p *PVTZProvider) record(ctx context.Context, ep *provider.PvtzEndpoint) error {
	if ep.Rr == "" {
		return fmt.Errorf("endpoint %s %s not found", ep.Rr, ep.Type)
	}
	records, err := p.SearchPVTZ(ctx, ep, true)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Rr == ep.Rr &&
			(ep.Type == "" || record.Type == ep.Type) {
			ep.Values = record.Values
			ep.Ttl = record.Ttl
			return nil
		}
	}
	// not found, setting result ep to empty
	ep.Values = []provider.PvtzValue{}
	return nil
}

func (p *PVTZProvider) create(recordType, rr, value string, ttl int) (*pvtz.AddZoneRecordResponse, error) {
	req := pvtz.CreateAddZoneRecordRequest()
	req.ZoneId = p.zoneId
	req.Type = recordType
	req.Rr = rr
	req.Ttl = requests.NewInteger(ttl)
	req.Value = value
	resp, err := p.client.AddZoneRecord(req)
	return resp, err
}

func (p *PVTZProvider) remark(recordId int64) error {
	req := pvtz.CreateUpdateRecordRemarkRequest()
	req.RecordId = requests.NewInteger(int(recordId))
	req.Remark = ZoneRecordRemark
	_, err := p.client.UpdateRecordRemark(req)
	return err
}

func (p *PVTZProvider) delete(recordId int64) error {
	req := pvtz.CreateDeleteZoneRecordRequest()
	req.RecordId = requests.NewInteger(int(recordId))
	_, err := p.client.DeleteZoneRecord(req)
	return err
}
