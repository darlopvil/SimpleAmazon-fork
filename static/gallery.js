// @license magnet:?xt=urn:btih:0b31508aeb0634b347b8270c7bee4d411b5d4109&dn=agpl-3.0.txt AGPL-3.0-or-later
//
// Anade flechas de desplazamiento a la galeria de la ficha de producto. Sin
// este script la galeria sigue siendo utilizable: es una tira con
// desplazamiento horizontal, solo que hay que arrastrarla a mano.
(function () {
    'use strict';

    var galeria = document.querySelector('.galeria');
    if (!galeria || galeria.children.length < 2) {
        return;
    }

    function desplazar(sentido) {
        // Cada imagen conserva su proporcion, de modo que sus anchos son
        // distintos y no sirve avanzar una cantidad fija. Se busca la imagen
        // siguiente o anterior a la que esta encuadrada y se desplaza hasta
        // ella.
        var items = galeria.querySelectorAll('.galeriaItem');
        var izquierdaGaleria = galeria.getBoundingClientRect().left;
        var actual = 0;

        for (var i = 0; i < items.length; i++) {
            var desfase = items[i].getBoundingClientRect().left - izquierdaGaleria;
            // Se considera encuadrada la primera cuyo borde no haya quedado
            // ya a la izquierda del area visible.
            if (desfase > -2) {
                actual = i;
                break;
            }
            actual = i;
        }

        var destino = actual + sentido;
        if (destino < 0) {
            destino = 0;
        }
        if (destino > items.length - 1) {
            destino = items.length - 1;
        }

        var salto = items[destino].getBoundingClientRect().left - izquierdaGaleria;
        galeria.scrollBy({ left: salto, behavior: 'smooth' });
    }

    function crearFlecha(texto, etiqueta, sentido) {
        var boton = document.createElement('button');
        boton.type = 'button';
        boton.className = 'galeriaFlecha';
        boton.textContent = texto;
        boton.setAttribute('aria-label', etiqueta);
        boton.addEventListener('click', function () {
            desplazar(sentido);
        });
        return boton;
    }

    var controles = document.createElement('div');
    controles.className = 'galeriaControles';
    controles.appendChild(crearFlecha('\u2039', 'Imagen anterior', -1));
    controles.appendChild(crearFlecha('\u203a', 'Imagen siguiente', 1));

    galeria.parentNode.insertBefore(controles, galeria.nextSibling);

    // El aviso de desplazamiento manual solo tiene sentido sin flechas.
    var aviso = document.querySelector('.galeriaAviso');
    if (aviso) {
        aviso.hidden = true;
    }
})();
// @license-end
