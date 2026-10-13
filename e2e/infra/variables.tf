variable "resource_group_name" {
  description = "Name of the existing Azure resource group"
  type        = string
}

variable "vm_name" {
  description = "Name for the VM and associated resources"
  type        = string
}

variable "location" {
  description = "Azure region for VM resources."
  type        = string
  default     = "germanywestcentral"
}

variable "vm_size" {
  description = "Azure VM size"
  type        = string
  default     = "Standard_D4as_v5"
}

variable "admin_username" {
  description = "SSH admin username"
  type        = string
  default     = "azureuser"
}

variable "ssh_public_key_path" {
  description = "Path to the SSH public key file"
  type        = string
}

variable "ssh_source_address_prefix" {
  description = "IPv4 CIDR (/24 to /32) allowed to reach the VMs on SSH, normally the provisioning machine's public IP as a /32"
  type        = string

  validation {
    condition     = can(regex("^((25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\\.){3}(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])/(2[4-9]|3[0-2])$", var.ssh_source_address_prefix))
    error_message = "ssh_source_address_prefix must be an IPv4 CIDR from /24 to /32, such as 203.0.113.7/32."
  }
}

variable "os_disk_size_gb" {
  description = "OS disk size in GB"
  type        = number
  default     = 50
}

variable "peer_vm_enabled" {
  description = "When true, provision a second VM on the same subnet for Firecracker two-runner e2e"
  type        = bool
  default     = false
}
