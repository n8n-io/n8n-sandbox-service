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
