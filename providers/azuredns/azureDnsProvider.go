package azuredns

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/go-autorest/autorest/to"
	"github.com/StackExchange/dnscontrol/models"
	"github.com/StackExchange/dnscontrol/providers"
	"github.com/StackExchange/dnscontrol/providers/diff"
	"github.com/pkg/errors"

	adns "github.com/Azure/azure-sdk-for-go/services/dns/mgmt/2018-05-01/dns"
	aauth "github.com/Azure/go-autorest/autorest/azure/auth"
)

type azureDnsProvider struct {
	zonesClient   *adns.ZonesClient
	recordsClient *adns.RecordSetsClient
	zones         map[string]*adns.Zone
	resourceGroup *string
}

func newAzureDnsDsp(conf map[string]string, metadata json.RawMessage) (providers.DNSServiceProvider, error) {
	return newAzureDns(conf, metadata)
}

func newAzureDns(m map[string]string, metadata json.RawMessage) (*azureDnsProvider, error) {
	subId, rg := m["SubscriptionID"], m["ResourceGroup"]

	zonesClient := adns.NewZonesClient(subId)
	recordsClient := adns.NewRecordSetsClient(subId)
	clientCredentialAuthorizer := aauth.NewClientCredentialsConfig(m["ClientID"], m["ClientSecret"], m["TenantID"])
	authorizer, authErr := clientCredentialAuthorizer.Authorizer()

	if authErr != nil {
		return nil, authErr
	}

	zonesClient.Authorizer = authorizer
	recordsClient.Authorizer = authorizer
	api := &azureDnsProvider{zonesClient: &zonesClient, recordsClient: &recordsClient, resourceGroup: to.StringPtr(rg)}
	err := api.getZones()
	if err != nil {
		return nil, err
	}
	return api, nil
}

var features = providers.DocumentationNotes{
	providers.CanUseAlias:            providers.Cannot("Only supported for Azure Resources. Not yet implemented"),
	providers.DocCreateDomains:       providers.Can(),
	providers.DocDualHost:            providers.Can("Azure does not permit modifying the existing NS records, only adding/removing additional records."),
	providers.DocOfficiallySupported: providers.Cannot(),
	providers.CanUsePTR:              providers.Can(),
	providers.CanUseSRV:              providers.Can(),
	providers.CanUseTXTMulti:         providers.Can(),
	providers.CanUseCAA:              providers.Can(),
	providers.CanUseRoute53Alias:     providers.Cannot(),
	providers.CanUseNAPTR:            providers.Cannot(),
	providers.CanUseSSHFP:            providers.Cannot(),
	providers.CanUseTLSA:             providers.Cannot(),
}

func init() {
	providers.RegisterDomainServiceProviderType("AZURE_DNS", newAzureDnsDsp, features)
}

func (a *azureDnsProvider) getZones() error {
	a.zones = make(map[string]*adns.Zone)

	ctx, cancel := context.WithTimeout(context.Background(), 6000*time.Second)
	defer cancel()
	zonesIterator, zonesErr := a.zonesClient.ListByResourceGroupComplete(ctx, *a.resourceGroup, to.Int32Ptr(100))
	if zonesErr != nil {
		return zonesErr
	}
	zonesResult := zonesIterator.Response()
	for _, z := range *zonesResult.Value {
		domain := strings.TrimSuffix(*z.Name, ".")
		a.zones[domain] = &z
	}

	return nil
}

type errNoExist struct {
	domain string
}

func (e errNoExist) Error() string {
	return fmt.Sprintf("Domain %s not found in you Azure account", e.domain)
}

func (a *azureDnsProvider) GetNameservers(domain string) ([]*models.Nameserver, error) {
	zone, ok := a.zones[domain]
	if !ok {
		return nil, errNoExist{domain}
	}

	var ns []*models.Nameserver
	if zone.ZoneProperties != nil {
		for _, azureNs := range *zone.ZoneProperties.NameServers {
			ns = append(ns, &models.Nameserver{Name: azureNs})
		}
	}
	return ns, nil
}

func (a *azureDnsProvider) GetDomainCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	err := dc.Punycode()

	if err != nil {
		return nil, err
	}

	var corrections []*models.Correction
	zone, ok := a.zones[dc.Name]
	if !ok {
		return nil, errNoExist{dc.Name}
	}

	records, err := a.fetchRecordSets(zone.Name)
	if err != nil {
		return nil, err
	}

	var existingRecords []*models.RecordConfig
	for _, set := range records {
		existingRecords = append(existingRecords, nativeToRecords(set, dc.Name)...)
	}

	models.PostProcessRecords(existingRecords)

	differ := diff.New(dc)
	namesToUpdate := differ.ChangedGroups(existingRecords)

	if len(namesToUpdate) == 0 {
		return nil, nil
	}

	updates := map[models.RecordKey][]*models.RecordConfig{}

	for k := range namesToUpdate {
		updates[k] = nil
		for _, rc := range dc.Records {
			if rc.Key() == k {
				updates[k] = append(updates[k], rc)
			}
		}
	}

	for k, recs := range updates {
		if len(recs) == 0 {
			var rrset *adns.RecordSet
			for _, r := range records {
				if strings.TrimSuffix(*r.RecordSetProperties.Fqdn, ".") == k.NameFQDN && azureRecordToRecordType(r.Type) == nativeToRecordType(to.StringPtr(k.Type)) {
					rrset = r
					break
				}
			}
			if rrset != nil {
				corrections = append(corrections,
					&models.Correction{
						Msg: strings.Join(namesToUpdate[k], "\n"),
						F: func() error {
							ctx, cancel := context.WithTimeout(context.Background(), 6000*time.Second)
							defer cancel()
							_, err := a.recordsClient.Delete(ctx, *a.resourceGroup, *zone.Name, *rrset.Name, azureRecordToRecordType(rrset.Type), "")
							// Artifically slow things down after a delete, as the API can take time to register it. The tests fail if we delete and then recheck too quickly.
							time.Sleep(25 * time.Millisecond)
							if err != nil {
								return err
							}
							return nil
						},
					})
			} else {
				return nil, fmt.Errorf("no record set found to delete. Name: '%s'. Type: '%s'", k.NameFQDN, k.Type)
			}
		} else {
			rrset, recordType := recordToNative(k, recs)
			var recordName string
			for _, r := range recs {
				i := int64(r.TTL)
				rrset.TTL = &i // TODO: make sure that ttls are consistent within a set
				recordName = r.Name
			}

			for _, r := range records {
				existingRecordType := azureRecordToRecordType(r.Type)
				changedRecordType := nativeToRecordType(to.StringPtr(k.Type))
				if strings.TrimSuffix(*r.RecordSetProperties.Fqdn, ".") == k.NameFQDN && (changedRecordType == adns.CNAME || existingRecordType == adns.CNAME) {
					if existingRecordType == adns.A || existingRecordType == adns.AAAA || changedRecordType == adns.A || changedRecordType == adns.AAAA { //CNAME cannot coexist with an A or AA
						corrections = append(corrections,
							&models.Correction{
								Msg: strings.Join(namesToUpdate[k], "\n"),
								F: func() error {
									ctx, cancel := context.WithTimeout(context.Background(), 6000*time.Second)
									defer cancel()
									_, err := a.recordsClient.Delete(ctx, *a.resourceGroup, *zone.Name, recordName, existingRecordType, "")
									// Artifically slow things down after a delete, as the API can take time to register it. The tests fail if we delete and then recheck too quickly.
									time.Sleep(25 * time.Millisecond)
									if err != nil {
										return err
									}
									return nil
								},
							})
					}
				}
			}

			corrections = append(corrections,
				&models.Correction{
					Msg: strings.Join(namesToUpdate[k], "\n"),
					F: func() error {
						ctx, cancel := context.WithTimeout(context.Background(), 6000*time.Second)
						defer cancel()
						_, err := a.recordsClient.CreateOrUpdate(ctx, *a.resourceGroup, *zone.Name, recordName, recordType, *rrset, "", "")
						// Artifically slow things down after a delete, as the API can take time to register it. The tests fail if we delete and then recheck too quickly.
						time.Sleep(25 * time.Millisecond)
						if err != nil {
							return err
						}
						return nil
					},
				})
		}
	}
	return corrections, nil
}

func nativeToRecordType(recordType *string) adns.RecordType {
	switch *recordType {
	case "A":
		return adns.A
	case "AAAA":
		return adns.AAAA
	case "CAA":
		return adns.CAA
	case "CNAME":
		return adns.CNAME
	case "MX":
		return adns.MX
	case "NS":
		return adns.NS
	case "PTR":
		return adns.PTR
	case "SRV":
		return adns.SRV
	case "TXT":
		return adns.TXT
	case "SOA":
		return adns.SOA
	default:
		panic(errors.Errorf("rc.String rtype %v unimplemented", *recordType))
	}
}

func azureRecordToRecordType(recordType *string) adns.RecordType {
	switch *recordType {
	case "Microsoft.Network/dnszones/A":
		return adns.A
	case "Microsoft.Network/dnszones/AAAA":
		return adns.AAAA
	case "Microsoft.Network/dnszones/CAA":
		return adns.CAA
	case "Microsoft.Network/dnszones/CNAME":
		return adns.CNAME
	case "Microsoft.Network/dnszones/MX":
		return adns.MX
	case "Microsoft.Network/dnszones/NS":
		return adns.NS
	case "Microsoft.Network/dnszones/PTR":
		return adns.PTR
	case "Microsoft.Network/dnszones/SRV":
		return adns.SRV
	case "Microsoft.Network/dnszones/TXT":
		return adns.TXT
	case "Microsoft.Network/dnszones/SOA":
		return adns.SOA
	default:
		panic(errors.Errorf("rc.String rtype %v unimplemented", *recordType))
	}
}

func nativeToRecords(set *adns.RecordSet, origin string) []*models.RecordConfig {
	var results []*models.RecordConfig
	switch rtype := *set.Type; rtype {
	case "Microsoft.Network/dnszones/A":
		for _, rec := range *set.ARecords {
			rc := &models.RecordConfig{TTL: uint32(*set.TTL)}
			rc.SetLabelFromFQDN(*set.Fqdn, origin)
			rc.Type = "A"
			_ = rc.SetTarget(*rec.Ipv4Address)
			results = append(results, rc)
		}
	case "Microsoft.Network/dnszones/AAAA":
		for _, rec := range *set.AaaaRecords {
			rc := &models.RecordConfig{TTL: uint32(*set.TTL)}
			rc.SetLabelFromFQDN(*set.Fqdn, origin)
			rc.Type = "AAAA"
			_ = rc.SetTarget(*rec.Ipv6Address)
			results = append(results, rc)
		}
	case "Microsoft.Network/dnszones/CNAME":
		rc := &models.RecordConfig{TTL: uint32(*set.TTL)}
		rc.SetLabelFromFQDN(*set.Fqdn, origin)
		rc.Type = "CNAME"
		_ = rc.SetTarget(*set.CnameRecord.Cname)
		results = append(results, rc)
	case "Microsoft.Network/dnszones/NS":
		for _, rec := range *set.NsRecords {
			rc := &models.RecordConfig{TTL: uint32(*set.TTL)}
			rc.SetLabelFromFQDN(*set.Fqdn, origin)
			rc.Type = "NS"
			_ = rc.SetTarget(*rec.Nsdname)
			results = append(results, rc)
		}
	case "Microsoft.Network/dnszones/PTR":
		for _, rec := range *set.PtrRecords {
			rc := &models.RecordConfig{TTL: uint32(*set.TTL)}
			rc.SetLabelFromFQDN(*set.Fqdn, origin)
			rc.Type = "PTR"
			_ = rc.SetTarget(*rec.Ptrdname)
			results = append(results, rc)
		}
	case "Microsoft.Network/dnszones/TXT":
		for _, rec := range *set.TxtRecords {
			rc := &models.RecordConfig{TTL: uint32(*set.TTL)}
			rc.SetLabelFromFQDN(*set.Fqdn, origin)
			rc.Type = "TXT"
			_ = rc.SetTargetTXTs(*rec.Value)
			results = append(results, rc)
		}
	case "Microsoft.Network/dnszones/MX":
		for _, rec := range *set.MxRecords {
			rc := &models.RecordConfig{TTL: uint32(*set.TTL)}
			rc.SetLabelFromFQDN(*set.Fqdn, origin)
			rc.Type = "MX"
			_ = rc.SetTargetMX(uint16(*rec.Preference), *rec.Exchange)
			results = append(results, rc)
		}
	case "Microsoft.Network/dnszones/SRV":
		for _, rec := range *set.SrvRecords {
			rc := &models.RecordConfig{TTL: uint32(*set.TTL)}
			rc.SetLabelFromFQDN(*set.Fqdn, origin)
			rc.Type = "SRV"
			_ = rc.SetTargetSRV(uint16(*rec.Priority), uint16(*rec.Weight), uint16(*rec.Port), *rec.Target)
			results = append(results, rc)
		}
	case "Microsoft.Network/dnszones/CAA":
		for _, rec := range *set.CaaRecords {
			rc := &models.RecordConfig{TTL: uint32(*set.TTL)}
			rc.SetLabelFromFQDN(*set.Fqdn, origin)
			rc.Type = "CAA"
			_ = rc.SetTargetCAA(uint8(*rec.Flags), *rec.Tag, *rec.Value)
			results = append(results, rc)
		}
	case "Microsoft.Network/dnszones/SOA":
	default:
		panic(errors.Errorf("rc.String rtype %v unimplemented", *set.Type))
	}
	return results
}

func recordToNative(recordKey models.RecordKey, recordConfig []*models.RecordConfig) (*adns.RecordSet, adns.RecordType) {
	recordSet := &adns.RecordSet{Type: to.StringPtr(recordKey.Type), RecordSetProperties: &adns.RecordSetProperties{}}
	for _, rec := range recordConfig {
		switch recordKey.Type {
		case "A":
			if recordSet.ARecords == nil {
				recordSet.ARecords = &[]adns.ARecord{}
			}
			*recordSet.ARecords = append(*recordSet.ARecords, adns.ARecord{Ipv4Address: to.StringPtr(rec.Target)})
		case "AAAA":
			if recordSet.AaaaRecords == nil {
				recordSet.AaaaRecords = &[]adns.AaaaRecord{}
			}
			*recordSet.AaaaRecords = append(*recordSet.AaaaRecords, adns.AaaaRecord{Ipv6Address: to.StringPtr(rec.Target)})
		case "CNAME":
			recordSet.CnameRecord = &adns.CnameRecord{Cname: to.StringPtr(rec.Target)}
		case "NS":
			if recordSet.NsRecords == nil {
				recordSet.NsRecords = &[]adns.NsRecord{}
			}
			*recordSet.NsRecords = append(*recordSet.NsRecords, adns.NsRecord{Nsdname: to.StringPtr(rec.Target)})
		case "PTR":
			if recordSet.PtrRecords == nil {
				recordSet.PtrRecords = &[]adns.PtrRecord{}
			}
			*recordSet.PtrRecords = append(*recordSet.PtrRecords, adns.PtrRecord{Ptrdname: to.StringPtr(rec.Target)})
		case "TXT":
			if recordSet.TxtRecords == nil {
				recordSet.TxtRecords = &[]adns.TxtRecord{}
			}
			*recordSet.TxtRecords = append(*recordSet.TxtRecords, adns.TxtRecord{Value: &rec.TxtStrings})
		case "MX":
			if recordSet.MxRecords == nil {
				recordSet.MxRecords = &[]adns.MxRecord{}
			}
			*recordSet.MxRecords = append(*recordSet.MxRecords, adns.MxRecord{Exchange: to.StringPtr(rec.Target), Preference: to.Int32Ptr(int32(rec.MxPreference))})
		case "SRV":
			if recordSet.SrvRecords == nil {
				recordSet.SrvRecords = &[]adns.SrvRecord{}
			}
			*recordSet.SrvRecords = append(*recordSet.SrvRecords, adns.SrvRecord{Target: to.StringPtr(rec.Target), Port: to.Int32Ptr(int32(rec.SrvPort)), Weight: to.Int32Ptr(int32(rec.SrvWeight)), Priority: to.Int32Ptr(int32(rec.SrvPriority))})
		case "CAA":
			if recordSet.CaaRecords == nil {
				recordSet.CaaRecords = &[]adns.CaaRecord{}
			}
			*recordSet.CaaRecords = append(*recordSet.CaaRecords, adns.CaaRecord{Value: to.StringPtr(rec.Target), Tag: to.StringPtr(rec.CaaTag), Flags: to.Int32Ptr(int32(rec.CaaFlag))})
		default:
			panic(errors.Errorf("rc.String rtype %v unimplemented", recordKey.Type))
		}
	}
	return recordSet, nativeToRecordType(to.StringPtr(recordKey.Type))
}

func (a *azureDnsProvider) fetchRecordSets(zoneName *string) ([]*adns.RecordSet, error) {
	if zoneName == nil || *zoneName == "" {
		return nil, nil
	}
	var records []*adns.RecordSet
	ctx, cancel := context.WithTimeout(context.Background(), 6000*time.Second)
	defer cancel()
	recordsIterator, recordsErr := a.recordsClient.ListAllByDNSZoneComplete(ctx, *a.resourceGroup, *zoneName, to.Int32Ptr(1000), "")
	if recordsErr != nil {
		return nil, recordsErr
	}
	recordsResult := recordsIterator.Response()

	for _, r := range *recordsResult.Value {
		record := r
		records = append(records, &record)
	}

	return records, nil
}

func (a *azureDnsProvider) EnsureDomainExists(domain string) error {
	if _, ok := a.zones[domain]; ok {
		return nil
	}
	fmt.Printf("Adding zone for %s to Azure dns account\n", domain)

	ctx, cancel := context.WithTimeout(context.Background(), 6000*time.Second)
	defer cancel()

	_, err := a.zonesClient.CreateOrUpdate(ctx, *a.resourceGroup, domain, adns.Zone{Location: to.StringPtr("global")}, "", "")
	if err != nil {
		return err
	}
	return nil
}
