variable "resource_group_name" {
  description = "Name of the existing Azure resource group"
  type        = string
}

variable "deployment_name" {
  description = "Name prefix for VMs and associated resources"
  type        = string
}

variable "location" {
  description = "Azure region for VM resources"
  type        = string
  default     = "germanywestcentral"
}

variable "vm_size" {
  description = "Azure VM size for each instance"
  type        = string
  default     = "Standard_D4s_v3"
}

variable "instance_count" {
  description = "Number of VMs in the compatibility cluster"
  type        = number
  default     = 3
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
  default     = 80
}
