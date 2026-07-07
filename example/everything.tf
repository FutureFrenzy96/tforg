terraform {
required_version=">= 1.5"
}
provider "aws" {
region=var.region
}
# The AWS region to deploy into.
variable "region" {
type=string
default="eu-west-1"
}
data "aws_ami" "ubuntu" {
most_recent=true
}
locals {
common_tags={team="platform"}
}
resource "aws_instance" "web" {
ami=data.aws_ami.ubuntu.id
tags=local.common_tags
}
output "id" {
value=aws_instance.web.id
}
