package aws

import (
	"fmt"
	"strings"

	"github.com/infracost/infracost/internal/schema"
	log "github.com/sirupsen/logrus"

	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
)

var defaultEC2InstanceMetricCount = 7

func GetInstanceRegistryItem() *schema.RegistryItem {
	return &schema.RegistryItem{
		Name: "aws_instance",
		Notes: []string{
			"Costs associated with marketplace AMIs are not supported.",
			"For non-standard Linux AMIs such as Windows and RHEL, the operating system should be specified in usage file.",
			"EC2 detailed monitoring assumes the standard 7 metrics and the lowest tier of prices for CloudWatch.",
			"If a root volume is not specified then an 8Gi gp2 volume is assumed.",
		},
		RFunc: NewInstance,
	}
}

func NewInstance(d *schema.ResourceData, u *schema.UsageData) *schema.Resource {
	tenancy := "Shared"
	if d.Get("tenancy").String() == "host" {
		log.Warnf("Skipping resource %s. Infracost currently does not support host tenancy for AWS EC2 instances", d.Address)
		return nil
	} else if d.Get("tenancy").String() == "dedicated" {
		tenancy = "Dedicated"
	}

	region := d.Get("region").String()
	subResources := make([]*schema.Resource, 0)
	subResources = append(subResources, newRootBlockDevice(d.Get("root_block_device.0"), region))
	subResources = append(subResources, newEbsBlockDevices(d.Get("ebs_block_device"), region)...)

	costComponents := []*schema.CostComponent{computeCostComponent(d, u, "on_demand", tenancy)}
	if d.Get("ebs_optimized").Bool() {
		costComponents = append(costComponents, ebsOptimizedCostComponent(d))
	}
	if d.Get("monitoring").Bool() {
		costComponents = append(costComponents, detailedMonitoringCostComponent(d))
	}
	c := cpuCreditsCostComponent(d)
	if c != nil {
		costComponents = append(costComponents, c)
	}

	return &schema.Resource{
		Name:           d.Address,
		SubResources:   subResources,
		CostComponents: costComponents,
	}
}

func computeCostComponent(d *schema.ResourceData, u *schema.UsageData, purchaseOption string, tenancy string) *schema.CostComponent {
	region := d.Get("region").String()
	instanceType := d.Get("instance_type").String()

	purchaseOptionLabel := map[string]string{
		"on_demand": "on-demand",
		"spot":      "spot",
	}[purchaseOption]

	osLabel := "Linux/UNIX"
	operatingSystem := "Linux"

	// Allow the operating system to be specified in the usage data until we can support it from the AMI directly.
	if u != nil && u.Get("operating_system").Exists() {
		os := strings.ToLower(u.Get("operating_system").String())
		switch os {
		case "windows":
			osLabel = "Windows"
			operatingSystem = "Windows"
		case "rhel":
			osLabel = "RHEL"
			operatingSystem = "RHEL"
		case "suse":
			osLabel = "SUSE"
			operatingSystem = "SUSE"
		default:
			if os != "linux" {
				log.Warnf("Unrecognized operating system %s, defaulting to Linux/UNIX", os)
			}
		}
	}

	var reservedIType, reservedTerm, reservedPaymentOption string
	if u != nil && u.Get("reserved_instance_type").Exists() {
		purchaseOptionLabel = "reserved"
		reservedIType = u.Get("reserved_instance_type").String()
		if u.Get("reserved_instance_term").Exists() {
			reservedTerm = u.Get("reserved_instance_term").String()
		}
		if u.Get("reserved_instance_payment_option").Exists() {
			reservedPaymentOption = u.Get("reserved_instance_payment_option").String()
		}
	}

	if reservedIType != "" {
		return reservedInstanceCostComponent(region, osLabel, purchaseOptionLabel, reservedIType, reservedTerm, reservedPaymentOption, tenancy, instanceType, operatingSystem, 1)
	}

	return &schema.CostComponent{
		Name:           fmt.Sprintf("Instance usage (%s, %s, %s)", osLabel, purchaseOptionLabel, instanceType),
		Unit:           "hours",
		UnitMultiplier: 1,
		HourlyQuantity: decimalPtr(decimal.NewFromInt(1)),
		ProductFilter: &schema.ProductFilter{
			VendorName:    strPtr("aws"),
			Region:        strPtr(region),
			Service:       strPtr("AmazonEC2"),
			ProductFamily: strPtr("Compute Instance"),
			AttributeFilters: []*schema.AttributeFilter{
				{Key: "instanceType", Value: strPtr(instanceType)},
				{Key: "tenancy", Value: strPtr(tenancy)},
				{Key: "operatingSystem", Value: strPtr(operatingSystem)},
				{Key: "preInstalledSw", Value: strPtr("NA")},
				{Key: "capacitystatus", Value: strPtr("Used")},
			},
		},
		PriceFilter: &schema.PriceFilter{
			PurchaseOption: &purchaseOption,
		},
	}
}

func reservedInstanceCostComponent(region, osLabel, purchaseOptionLabel, reservedType, reservedTerm, reservedPaymentOption, tenancy, instanceType, operatingSystem string, count int64) *schema.CostComponent {
	reservedTermName := map[string]string{
		"1_year": "1yr",
		"3_year": "3yr",
	}[reservedTerm]

	reservedPaymentOptionName := map[string]string{
		"no_upfront":      "No Upfront",
		"partial_upfront": "Partial Upfront",
		"all_upfront":     "All Upfront",
	}[reservedPaymentOption]

	return &schema.CostComponent{
		Name:           fmt.Sprintf("Instance usage (%s, %s, %s)", osLabel, purchaseOptionLabel, instanceType),
		Unit:           "hours",
		UnitMultiplier: 1,
		HourlyQuantity: decimalPtr(decimal.NewFromInt(count)),
		ProductFilter: &schema.ProductFilter{
			VendorName:    strPtr("aws"),
			Region:        strPtr(region),
			Service:       strPtr("AmazonEC2"),
			ProductFamily: strPtr("Compute Instance"),
			AttributeFilters: []*schema.AttributeFilter{
				{Key: "instanceType", Value: strPtr(instanceType)},
				{Key: "tenancy", Value: strPtr(tenancy)},
				{Key: "operatingSystem", Value: strPtr(operatingSystem)},
				{Key: "preInstalledSw", Value: strPtr("NA")},
				{Key: "capacitystatus", Value: strPtr("Used")},
			},
		},
		PriceFilter: &schema.PriceFilter{
			StartUsageAmount:   strPtr("0"),
			TermOfferingClass:  &reservedType,
			TermLength:         &reservedTermName,
			TermPurchaseOption: &reservedPaymentOptionName,
		},
	}
}

func ebsOptimizedCostComponent(d *schema.ResourceData) *schema.CostComponent {
	region := d.Get("region").String()
	instanceType := d.Get("instance_type").String()

	return &schema.CostComponent{
		Name:                 "EBS-optimized usage",
		Unit:                 "hours",
		UnitMultiplier:       1,
		HourlyQuantity:       decimalPtr(decimal.NewFromInt(1)),
		IgnoreIfMissingPrice: true,
		ProductFilter: &schema.ProductFilter{
			VendorName:    strPtr("aws"),
			Region:        strPtr(region),
			Service:       strPtr("AmazonEC2"),
			ProductFamily: strPtr("Compute Instance"),
			AttributeFilters: []*schema.AttributeFilter{
				{Key: "instanceType", Value: strPtr(instanceType)},
				{Key: "usagetype", ValueRegex: strPtr("/EBSOptimized/")},
			},
		},
	}
}

func detailedMonitoringCostComponent(d *schema.ResourceData) *schema.CostComponent {
	region := d.Get("region").String()

	return &schema.CostComponent{
		Name:                 "EC2 detailed monitoring",
		Unit:                 "metrics",
		UnitMultiplier:       1,
		MonthlyQuantity:      decimalPtr(decimal.NewFromInt(int64(defaultEC2InstanceMetricCount))),
		IgnoreIfMissingPrice: true,
		ProductFilter: &schema.ProductFilter{
			VendorName:    strPtr("aws"),
			Region:        strPtr(region),
			Service:       strPtr("AmazonCloudWatch"),
			ProductFamily: strPtr("Metric"),
		},
		PriceFilter: &schema.PriceFilter{
			StartUsageAmount: strPtr("0"),
		},
	}
}

func cpuCreditsCostComponent(d *schema.ResourceData) *schema.CostComponent {
	region := d.Get("region").String()
	instanceType := d.Get("instance_type").String()

	cpuCredits := d.Get("credit_specification.0.cpu_credits").String()
	if cpuCredits == "" && (strings.HasPrefix(instanceType, "t3.") || strings.HasPrefix(instanceType, "t4g.")) {
		cpuCredits = "unlimited"
	}

	if cpuCredits != "unlimited" {
		return nil
	}

	prefix := strings.SplitN(instanceType, ".", 2)[0]

	return &schema.CostComponent{
		Name:           "CPU credits",
		Unit:           "vCPU-hours",
		UnitMultiplier: 1,
		ProductFilter: &schema.ProductFilter{
			VendorName:    strPtr("aws"),
			Region:        strPtr(region),
			Service:       strPtr("AmazonEC2"),
			ProductFamily: strPtr("CPU Credits"),
			AttributeFilters: []*schema.AttributeFilter{
				{Key: "operatingSystem", Value: strPtr("Linux")},
				{Key: "usagetype", ValueRegex: strPtr(fmt.Sprintf("/CPUCredits:%s$/", prefix))},
			},
		},
	}
}

func newRootBlockDevice(d gjson.Result, region string) *schema.Resource {
	return newEbsBlockDevice("root_block_device", d, region)
}

func newEbsBlockDevices(d gjson.Result, region string) []*schema.Resource {
	resources := make([]*schema.Resource, 0)
	for i, data := range d.Array() {
		name := fmt.Sprintf("ebs_block_device[%d]", i)
		resources = append(resources, newEbsBlockDevice(name, data, region))
	}
	return resources
}

func newEbsBlockDevice(name string, d gjson.Result, region string) *schema.Resource {
	volumeAPIName := "gp2"
	if d.Get("volume_type").Exists() {
		volumeAPIName = d.Get("volume_type").String()
	}

	gbVal := decimal.NewFromInt(int64(defaultVolumeSize))
	if d.Get("volume_size").Exists() {
		gbVal = decimal.NewFromFloat(d.Get("volume_size").Float())
	}

	iopsVal := decimal.Zero
	if d.Get("iops").Exists() {
		iopsVal = decimal.NewFromFloat(d.Get("iops").Float())
	}

	var unknown *decimal.Decimal

	return &schema.Resource{
		Name:           name,
		CostComponents: ebsVolumeCostComponents(region, volumeAPIName, unknown, gbVal, iopsVal, unknown),
	}
}
