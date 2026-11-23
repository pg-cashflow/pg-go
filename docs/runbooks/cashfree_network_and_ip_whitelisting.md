# Cashfree Network and IP Whitelisting Runbook

## 1. Overview
This runbook defines the network configuration rules for Cashfree integration.
Follow these steps to prevent rejected API calls and missed webhooks.

---

## 2. Cashfree Service Requirements Matrix

| Service | Environment | Outbound IP Whitelist | Authentication Type |
|---|---|---|---|
| Payment Gateway (Orders/Payments) | Sandbox & Production | Not Required | Client ID and Secret (`X-Client-Id`, `X-Client-Secret`) |
| Secure ID (KYC / DigiLocker) | Sandbox & Production | Mandatory | Client ID, Secret, and Outbound IPv4 Whitelist |
| Payouts API | Production | Mandatory | IPv4 Whitelist OR RSA Public Key Signature |
| Inbound Webhooks | Sandbox & Production | Firewall Ingress Allow | HMAC-SHA256 Signature (`x-webhook-signature`) |

---

## 3. Server Outbound IP Configuration

### 3.1 Static IPv4 Assignment
Cashfree supports only public IPv4 addresses.
Cashfree does not support IPv6.

1. Ensure the production host uses a dedicated, static IPv4 address.
2. In single-VPS setups, record the static public IPv4 address.
3. In cloud setups (AWS, GCP, DigitalOcean), bind an Elastic IP or Cloud NAT gateway.
4. Verify the outbound public IP address from the server terminal:
   ```bash
   curl -4 ifconfig.me
   ```

### 3.2 Add IP to Cashfree Merchant Dashboard
1. Log in to the [Cashfree Merchant Dashboard](https://merchant.cashfree.com).
2. For Secure ID (KYC / DigiLocker):
   - Navigate to **Verification Suite** -> **Developers** -> **IP Whitelist**.
   - Click **Add IP Address**.
   - Enter the server static public IPv4 address.
   - Save the configuration.
3. For Payouts:
   - Navigate to **Payouts** -> **Developers** -> **IP Whitelist**.
   - Add the server static public IPv4 address.
   - Alternatively, configure an RSA public key in **Public Key Authentication**.

---

## 4. Inbound Webhook Firewall Rules

### 4.1 Cashfree Webhook IP Ranges
Configure the host firewall (UFW, AWS Security Group, or Cloudflare) to permit inbound HTTPS traffic from Cashfree webhook dispatchers:

```
52.66.101.190
3.109.91.50
13.235.130.6
```

### 4.2 UFW Command Example (Single VPS)
Run these commands on the production Ubuntu/Debian host:
```bash
sudo ufw allow from 52.66.101.190 to any port 443 proto tcp comment 'Cashfree Webhook 1'
sudo ufw allow from 3.109.91.50 to any port 443 proto tcp comment 'Cashfree Webhook 2'
sudo ufw allow from 13.235.130.6 to any port 443 proto tcp comment 'Cashfree Webhook 3'
sudo ufw reload
```

---

## 5. Verification and Health Checks

1. Verify outbound connectivity to Cashfree Verification Suite:
   ```bash
   curl -I https://api.cashfree.com/verification/offline-aadhaar/otp
   ```
   An authorized or structured error response indicates network reachability.
   A connection timeout or 403 Forbidden indicates an IP whitelist failure.
2. Monitor application logs for Cashfree API error codes:
   ```bash
   grep -i "ip_not_whitelisted" /var/log/pg-cashflow/server.log
   ```
3. If an IP error occurs, re-verify the active outbound IP with `curl -4 ifconfig.me`.
