variable "cidr" {
type=string
}
resource "aws_subnet" "private" {
cidr_block=var.cidr
}
output "subnet_id" {
value=aws_subnet.private.id
}
