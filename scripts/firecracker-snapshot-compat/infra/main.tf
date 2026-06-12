terraform {
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.73"
    }
  }
}

provider "azurerm" {
  features {}

  resource_provider_registrations = "none"
}

data "azurerm_resource_group" "compat" {
  name = var.resource_group_name
}

locals {
  tags = {
    purpose = "firecracker-snapshot-compat"
  }
}

resource "azurerm_virtual_network" "compat" {
  name                = "${var.deployment_name}-vnet"
  address_space       = ["10.1.0.0/16"]
  location            = var.location
  resource_group_name = data.azurerm_resource_group.compat.name
  tags                = local.tags
}

resource "azurerm_subnet" "compat" {
  name                 = "${var.deployment_name}-subnet"
  resource_group_name  = data.azurerm_resource_group.compat.name
  virtual_network_name = azurerm_virtual_network.compat.name
  address_prefixes     = ["10.1.1.0/24"]
}

resource "azurerm_network_security_group" "compat" {
  name                = "${var.deployment_name}-nsg"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.compat.name

  security_rule {
    name                       = "SSH"
    priority                   = 1001
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }

  tags = local.tags
}

resource "azurerm_subnet_network_security_group_association" "compat" {
  subnet_id                 = azurerm_subnet.compat.id
  network_security_group_id = azurerm_network_security_group.compat.id
}

resource "azurerm_public_ip" "instance" {
  count               = var.instance_count
  name                = "${var.deployment_name}-${count.index}-pip"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.compat.name
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = local.tags
}

resource "azurerm_network_interface" "instance" {
  count               = var.instance_count
  name                = "${var.deployment_name}-${count.index}-nic"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.compat.name
  tags                = local.tags

  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.compat.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.instance[count.index].id
  }
}

resource "azurerm_network_interface_security_group_association" "instance" {
  count                     = var.instance_count
  network_interface_id      = azurerm_network_interface.instance[count.index].id
  network_security_group_id = azurerm_network_security_group.compat.id
}

resource "azurerm_linux_virtual_machine" "instance" {
  count               = var.instance_count
  name                = "${var.deployment_name}-${count.index}"
  resource_group_name = data.azurerm_resource_group.compat.name
  location            = var.location
  size                = var.vm_size
  admin_username      = var.admin_username

  network_interface_ids = [
    azurerm_network_interface.instance[count.index].id,
  ]

  admin_ssh_key {
    username   = var.admin_username
    public_key = file(var.ssh_public_key_path)
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "Premium_LRS"
    disk_size_gb         = var.os_disk_size_gb
  }

  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }

  tags = local.tags
}
