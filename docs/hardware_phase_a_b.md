# PG Facility Engineering & Hardware Guidance: Phase A / B

This guide details actionable facility and hardware upgrades for Andhra Pradesh / Indian student and co-living PGs, prioritized according to the retention and ROI stack: **Water & Cleanliness Reliability > Visibility & Sub-metering > Common-Area Automation**.

---

## 1. Water Reliability & RO Reject Diversion (Day One)

### Dual-RO Purifier Redundancy
- **The Risk**: Water outages or broken purifiers are the #1 trigger for student protests and rapid churn (as seen across Hyderabad, Surat, and Punjab hostels).
- **Architecture**:
  - Install at least **two independent RO/water purifier units** on separate electrical circuits and plumbing lines.
  - If Unit 1 requires filter service, Unit 2 ensures uninterrupted drinking water supply.
  - Maintain a physical and in-app filter-change log with a maximum 24-hour repair SLA.

### RO Reject-Water Diversion Kit (< ₹200 / unit)
- **The Physics**: Standard commercial RO purifiers waste roughly **3 to 4 litres of reject water** for every 1 litre of purified drinking water. For a 50-bed PG consuming 150 L of drinking water daily, that is **450–600 litres of potable water wasted every day**.
- **The Plumbing Fix**:
  - Connect a 1/4" food-grade tube to the RO reject outlet.
  - Route the tube into a dedicated 200-litre blue drum (with a tap) located near the wash area / kitchen.
  - Use this water exclusively for:
    1. Floor mopping and corridor washing.
    2. Common-area toilet flushing.
    3. Initial utensil rinsing.
- **ROI**: Pure operational savings with zero tenant behavioral friction.

### Overhead Tank-Level Sensor (₹1,500 – ₹3,000)
- Install a basic ultrasonic or float-based wireless tank-level indicator with an alert buzzer/SMS when the overhead tank drops below 25%.
- Prevents morning dry-tank emergencies.

### Rainwater Harvesting (RWH) Compliance (AP WALTA Act 2002)
- Andhra Pradesh Water, Land and Trees Act (WALTA) Section 17(1) mandates RWH structures for buildings $\ge 200\text{ m}^2$ (or $300\text{ m}^2$ under local municipal bylaws).
- Verify exact plot threshold with Rajahmundry Municipal Corporation.
- A recharge pit directly replenishes your borewell water table, protecting long-term borewell yield during dry summer months.

---

## 2. Electricity: Phase A (Zero-IoT & Sub-Metering)

### Visibility-First Economics
- Studies across dormitories show that social comparison and consumption visibility alone deliver **15% to 30% sustained reductions** in electricity waste without purchasing smart switches or IoT devices.

### Sub-Metering Architecture
- Install basic digital sub-meters (~₹800 to ₹1,200 per room) or DIN-rail energy meters on each room's distribution board.
- **Quota Model**:
  - Include a fair baseline quota in the rent (e.g. 50 units/kWh per month for a double-sharing room).
  - Warden records the meter reading once a month via the Warden App.
  - The app calculates:
    $$\text{Billable Units} = \max(0, \text{Delta} - 50)$$
  - Excess units generate an `electricity` due line at the property tariff (e.g. ₹10/unit).
- **Safety Bounds (P0 Invariant)**:
  - The app enforces $R_{new} \ge R_{prev}$ and caps delta at 400 kWh to prevent accidental typo billing.

### BLDC Ceiling Fans (High ROI)
- Replace conventional 75W induction ceiling fans with 28W Brushless DC (BLDC) fans (Atomberg, Havells, Crompton; ~₹2,800–₹3,200).
- **Payback Period**:
  - Standard fan: $75\text{W} \times 12\text{ hours/day} \times 365\text{ days} = 328.5\text{ kWh/year}$.
  - BLDC fan: $28\text{W} \times 12\text{ hours/day} \times 365\text{ days} = 122.6\text{ kWh/year}$.
  - Savings: $\approx 206\text{ kWh/year}$ per fan $\times$ ₹8–10/kWh = **₹1,600 to ₹2,000 saved per room/year**.
  - Capital payback in **1.5 to 2 years**.

### Corridor Dusk-to-Dawn LDR Sensors (~₹150)
- Install photocell/LDR sensors on exterior, staircase, and terrace lights so bulbs automatically turn off at sunrise.

---

## 3. Electricity: Phase B (Common Area Automation)

- **Do NOT install PIR motion sensors inside bedrooms**: sleeping occupants remain still, causing false switch-offs that prompt tenants to bypass sensors.
- **Common Area Motion Sensors (~₹400–₹700)**:
  - Install PIR motion sensors in common corridors, staircases, and shared washrooms (with a 3-minute off-delay).
- **Key-Card Master Energy Savers (~₹800–₹1,200/room)**:
  - Hotel-style key-card slots near the room door: removing the room key when leaving de-energizes room sockets, lights, and fans. Works reliably without internet or false triggers.

---

## 4. WiFi & Bandwidth Policy

- Never throttle bandwidth as a gamification penalty: tenants perceive slow internet as a service breakdown, breaking trust.
- Provide reliable, symmetric bandwidth per floor with bandwidth limits managed at the router (e.g. MikroTik queue limits per room).
- Reserve gamification for timely pay, food waste prevention, and cleanliness.
