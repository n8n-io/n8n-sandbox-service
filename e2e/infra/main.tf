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

data "azurerm_resource_group" "e2e" {
  name = var.resource_group_name
}

locals {
  tags = {
    purpose = "sandbox-service-e2e"
  }
}

resource "azurerm_virtual_network" "e2e" {
  name                = "${var.vm_name}-vnet"
  address_space       = ["10.0.0.0/16"]
  location            = var.location
  resource_group_name = data.azurerm_resource_group.e2e.name
  tags                = local.tags
}

resource "azurerm_subnet" "e2e" {
  name                 = "${var.vm_name}-subnet"
  resource_group_name  = data.azurerm_resource_group.e2e.name
  virtual_network_name = azurerm_virtual_network.e2e.name
  address_prefixes     = ["10.0.1.0/24"]
}

resource "azurerm_public_ip" "e2e" {
  name                = "${var.vm_name}-pip"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.e2e.name
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = local.tags
}

resource "azurerm_network_security_group" "e2e" {
  name                = "${var.vm_name}-nsg"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.e2e.name

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

resource "azurerm_network_interface" "e2e" {
  name                = "${var.vm_name}-nic"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.e2e.name
  tags                = local.tags

  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.e2e.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.e2e.id
  }
}

resource "azurerm_network_interface_security_group_association" "e2e" {
  network_interface_id      = azurerm_network_interface.e2e.id
  network_security_group_id = azurerm_network_security_group.e2e.id
}

resource "azurerm_linux_virtual_machine" "e2e" {
  name                = var.vm_name
  resource_group_name = data.azurerm_resource_group.e2e.name
  location            = var.location
  size                = var.vm_size
  admin_username      = var.admin_username

  network_interface_ids = [
    azurerm_network_interface.e2e.id,
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
