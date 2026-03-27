import type { ComponentChildren } from "preact";

// ── Toggle ────────────────────────────────────────────────────────────────────
interface ToggleProps {
  label: string;
  description?: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
}

export function Toggle({ label, description, checked, onChange, disabled }: ToggleProps) {
  return (
    <div className="cfg-field">
      <div className="cfg-field-row">
        <div className="cfg-field-text">
          <span className="cfg-field-label">{label}</span>
          {description && <span className="cfg-field-desc">{description}</span>}
        </div>
        <button
          className={`cfg-toggle ${checked ? "cfg-toggle-on" : ""}`}
          onClick={() => !disabled && onChange(!checked)}
          disabled={disabled}
          role="switch"
          aria-checked={checked}
        >
          <span className="cfg-toggle-thumb" />
        </button>
      </div>
    </div>
  );
}

// ── Slider ────────────────────────────────────────────────────────────────────
interface SliderProps {
  label: string;
  description?: string;
  value: number;
  min: number;
  max: number;
  step?: number;
  onChange: (v: number) => void;
  unit?: string;
  disabled?: boolean;
}

export function Slider({ label, description, value, min, max, step = 1, onChange, unit, disabled }: SliderProps) {
  return (
    <div className="cfg-field">
      <div className="cfg-field-text">
        <span className="cfg-field-label">{label}</span>
        {description && <span className="cfg-field-desc">{description}</span>}
      </div>
      <div className="cfg-slider-row">
        <input
          type="range"
          className="cfg-slider"
          min={min}
          max={max}
          step={step}
          value={value}
          disabled={disabled}
          onInput={(e) => onChange(Number((e.target as HTMLInputElement).value))}
        />
        <span className="cfg-slider-value">{value}{unit ?? ""}</span>
      </div>
    </div>
  );
}

// ── Select ────────────────────────────────────────────────────────────────────
interface SelectOption {
  value: string;
  label: string;
}

interface SelectProps {
  label: string;
  description?: string;
  value: string;
  options: SelectOption[];
  onChange: (v: string) => void;
  disabled?: boolean;
}

export function Select({ label, description, value, options, onChange, disabled }: SelectProps) {
  return (
    <div className="cfg-field">
      <div className="cfg-field-text">
        <span className="cfg-field-label">{label}</span>
        {description && <span className="cfg-field-desc">{description}</span>}
      </div>
      <select
        className="cfg-select"
        value={value}
        disabled={disabled}
        onChange={(e) => onChange((e.target as HTMLSelectElement).value)}
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>{o.label}</option>
        ))}
      </select>
    </div>
  );
}

// ── TextInput ─────────────────────────────────────────────────────────────────
interface TextInputProps {
  label: string;
  description?: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  disabled?: boolean;
  type?: string;
}

export function TextInput({ label, description, value, onChange, placeholder, disabled, type }: TextInputProps) {
  return (
    <div className="cfg-field">
      <div className="cfg-field-text">
        <span className="cfg-field-label">{label}</span>
        {description && <span className="cfg-field-desc">{description}</span>}
      </div>
      <input
        type={type ?? "text"}
        className="cfg-input"
        value={value}
        placeholder={placeholder}
        disabled={disabled}
        onInput={(e) => onChange((e.target as HTMLInputElement).value)}
      />
    </div>
  );
}

// ── NumberInput ────────────────────────────────────────────────────────────────
interface NumberInputProps {
  label: string;
  description?: string;
  value: number;
  onChange: (v: number) => void;
  min?: number;
  max?: number;
  disabled?: boolean;
  unit?: string;
}

export function NumberInput({ label, description, value, onChange, min, max, disabled, unit }: NumberInputProps) {
  return (
    <div className="cfg-field">
      <div className="cfg-field-row">
        <div className="cfg-field-text">
          <span className="cfg-field-label">{label}</span>
          {description && <span className="cfg-field-desc">{description}</span>}
        </div>
        <div className="cfg-number-row">
          <input
            type="number"
            className="cfg-input cfg-input-sm"
            value={value}
            min={min}
            max={max}
            disabled={disabled}
            onInput={(e) => onChange(Number((e.target as HTMLInputElement).value))}
          />
          {unit && <span className="cfg-slider-value">{unit}</span>}
        </div>
      </div>
    </div>
  );
}

// ── ConfigSection ─────────────────────────────────────────────────────────────
interface ConfigSectionProps {
  title: string;
  description?: string;
  children: ComponentChildren;
}

export function ConfigSection({ title, description, children }: ConfigSectionProps) {
  return (
    <div className="cfg-section">
      <div className="cfg-section-header">
        <h3 className="cfg-section-title">{title}</h3>
        {description && <p className="cfg-section-desc">{description}</p>}
      </div>
      <div className="cfg-section-body">{children}</div>
    </div>
  );
}
