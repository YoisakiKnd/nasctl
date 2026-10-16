import './styles.css';
import { mount } from 'svelte';
import { loadTheme } from './lib/theme.js';
import App from './App.svelte';

loadTheme();
mount(App, { target: document.getElementById('app') });
