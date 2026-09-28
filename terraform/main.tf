# `terraform apply` = kind cluster + operator image loaded + Helm release.
# Everything the e2e suite needs except the demo workloads (deploy/demo.yaml).

terraform {
  required_version = ">= 1.6"
  required_providers {
    kind = {
      source  = "tehcyx/kind"
      version = "~> 0.9"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.17"
    }
  }
}

variable "cluster_name" {
  type    = string
  default = "serviceguard"
}

variable "image" {
  description = "Operator image, already built locally (docker build -t serviceguard:dev .)."
  type        = string
  default     = "serviceguard:dev"
}

resource "kind_cluster" "this" {
  name           = var.cluster_name
  wait_for_ready = true

  kind_config {
    kind        = "Cluster"
    api_version = "kind.x-k8s.io/v1alpha4"
    node {
      role = "control-plane"
    }
  }
}

# kind nodes can't pull a locally built image; side-load it. Re-runs when the
# image tag or the cluster changes.
resource "terraform_data" "load_image" {
  triggers_replace = [var.image, kind_cluster.this.id]

  provisioner "local-exec" {
    command = "kind load docker-image ${var.image} --name ${kind_cluster.this.name}"
  }
}

provider "helm" {
  kubernetes {
    host                   = kind_cluster.this.endpoint
    client_certificate     = kind_cluster.this.client_certificate
    client_key             = kind_cluster.this.client_key
    cluster_ca_certificate = kind_cluster.this.cluster_ca_certificate
  }
}

resource "helm_release" "operator" {
  name             = "serviceguard"
  chart            = "${path.module}/../charts/serviceguard-operator"
  namespace        = "serviceguard-system"
  create_namespace = true
  wait             = true
  timeout          = 300

  set {
    name  = "image.repository"
    value = split(":", var.image)[0]
  }
  set {
    name  = "image.tag"
    value = split(":", var.image)[1]
  }

  depends_on = [terraform_data.load_image]
}

output "kubeconfig_path" {
  value = kind_cluster.this.kubeconfig_path
}
