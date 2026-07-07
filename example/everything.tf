terraform {
required_version=">= 1.5"
}
# The AWS region to deploy into.
variable "region" {
type=string
default="eu-west-1"
}
variable "instance_count" {
type=number
default=2
}
resource "aws_instance" "web" {
ami="ami-123"
}
output "id" {
value=aws_instance.web.id
}
